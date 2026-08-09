package assertobs

// prom_query.go — shared access to in-cluster HTTP APIs behind a Service, for
// the `prom-metrics` / `alert-eval` diagnostics and the assert-* gates.
//
// It reaches them via `kubectl port-forward`, NOT the apiserver Service proxy
// (`kubectl get --raw /api/v1/namespaces/.../services/.../proxy/...`). On
// LKE-Enterprise the services/proxy subresource is webhook-denied even for the
// cluster-admin `lke-admin` ServiceAccount the health checks run as — the proxy
// fetch fails with "RBAC: access denied" despite an SSAR saying it's allowed. The
// pods/portforward subresource IS allowed, so port-forward is the portable path.
//
// WithPrometheus opens ONE port-forward for the lifetime of a command's queries
// (alert-eval runs 20+), rather than a fresh kubectl per query.
//
// withForwardedAPI is the same machinery with the far end named rather than
// assumed: assert-openbao-audit queries Loki over it (loki_query.go). Everything
// here — the ":0" port handshake, the drain goroutine, the warm-up, the non-2xx
// arm — was learned against Prometheus and is worth exactly as much against any
// other in-cluster HTTP API, so it is shared rather than copied.

import (
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/harborauth"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/portfwd"
)

// forwardedService describes the far end of a port-forward: what to call it in
// errors, which path to warm the tunnel against, and any headers every request
// must carry. Zero readyPath falls back to Prometheus's "/-/ready".
type forwardedService struct {
	name      string            // "Prometheus" / "Loki" — appears in error text
	readyPath string            // warm-up probe path (any HTTP status counts; we only need the dial to land)
	headers   map[string]string // sent on every request (Loki's X-Scope-OrgID)
}

// WithPrometheus opens a single kubectl port-forward to the Prometheus named by
// promSpec ("<namespace>/<service>:<port>"), invokes fn with a getter bound to
// it, and tears the forward down on return. Package var so tests can seam it.
// EXPORTED as a SEAM, not just a function: the rotation-health and reconciler
// assert lanes still in internal/cli open Prometheus through it, and their tests
// stub it. One place decides how this repo reaches Prometheus.
var WithPrometheus = func(promSpec string, fn func(get func(apiPath string) ([]byte, error)) error) error {
	return withForwardedAPI(promSpec, forwardedService{name: "Prometheus", readyPath: "/-/ready"}, fn)
}

// withForwardedAPI is WithPrometheus's body, generalized over the service on the
// other end. See forwardedService.
func withForwardedAPI(spec string, target forwardedService, fn func(get func(apiPath string) ([]byte, error)) error) error {
	if target.name == "" {
		target.name = "service"
	}
	if target.readyPath == "" {
		target.readyPath = "/-/ready"
	}
	ns, svc, port, err := parsePromSpec(spec)
	if err != nil {
		return err
	}
	// Local port ":0" → kubectl picks a free port and announces it on stdout.
	cmd := exec.Command("kubectl", "port-forward", "-n", ns, "svc/"+svc, ":"+port)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("kubectl port-forward: %w", err)
	}
	// Kill + reap on return; killing closes stdout, which unblocks the reader
	// goroutine in portfwd.ReadForwardPortTimeout (no leak).
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	localPort, err := portfwd.ReadForwardPortTimeout(stdout, portfwd.ForwardEstablishTimeout)
	if err != nil {
		return err
	}
	// Keep draining stdout: once we stop reading, kubectl's per-connection log
	// lines would fill the pipe buffer and block its writer. The port reader has
	// finished, so this is the sole reader now.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	client := &http.Client{Timeout: 15 * time.Second}
	base := "http://127.0.0.1:" + localPort
	// The listener is announced up, but the first dial can still race the tunnel;
	// warm up (bounded) before handing the getter to fn.
	if err := warmUpForward(client, base+target.readyPath); err != nil {
		return err
	}
	// A non-2xx is an ERROR, not a body. This returned any response with err ==
	// nil, so a 503, a 500, or Prometheus's own {"status":"error"} envelope
	// reached callers as if it were data — and every caller then read it as an
	// answer about the cluster:
	//
	//   alert-eval  a failed metric-name fetch left `known` empty, which makes
	//               exprMetricsExist return true for every rule, so the DEAD?
	//               count was structurally 0 and --strict could not fail on it.
	//               #242 closed the transport path; this closes the body.
	//   prom-rules  promRulesJSON has no status field, so an error envelope
	//               unmarshals cleanly with zero groups → "All Prometheus rule
	//               groups evaluated without errors". Zero rules loaded — the
	//               exact ruleSelector regression monitoring-label-guard exists
	//               for — read identically color.Green.
	//
	// The body is carried into the error (truncated) so callers keep Prometheus's
	// own explanation instead of just a status number.
	get := func(apiPath string) ([]byte, error) {
		req, err := http.NewRequest(http.MethodGet, base+apiPath, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range target.headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("GET %s: reading response: %w", apiPath, readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("GET %s: %s returned HTTP %d: %s",
				apiPath, target.name, resp.StatusCode, harborauth.TruncateForError(body))
		}
		return body, nil
	}
	return fn(get)
}

// warmUpForward retries the target's readiness URL until the tunnel accepts a
// connection (or a short budget elapses), so the first real query doesn't race
// the port-forward coming up. Any HTTP status counts — a 404 from a gateway that
// doesn't serve the probe path still proves the tunnel carries traffic.
func warmUpForward(client *http.Client, readyURL string) error {
	var lastErr error
	for i := 0; i < 15; i++ {
		resp, err := client.Get(readyURL)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("port-forward tunnel never became ready: %w", lastErr)
}

// parsePromSpec splits "<namespace>/<service>:<port>".
func parsePromSpec(spec string) (ns, svc, port string, err error) {
	ns, svcPort, ok := strings.Cut(spec, "/")
	if !ok {
		return "", "", "", fmt.Errorf("prom spec must be <namespace>/<service>:<port>, got %q", spec)
	}
	svc, port, ok = strings.Cut(svcPort, ":")
	if !ok || svc == "" || port == "" {
		return "", "", "", fmt.Errorf("prom spec must be <namespace>/<service>:<port>, got %q", spec)
	}
	return ns, svc, port, nil
}

// truncateForError keeps an error message readable when the body is an HTML error
