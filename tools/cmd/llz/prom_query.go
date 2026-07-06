package main

// prom_query.go — shared access to the in-cluster Prometheus HTTP API for the
// `prom-metrics` and `alert-eval` diagnostics.
//
// It reaches Prometheus via `kubectl port-forward`, NOT the apiserver Service
// proxy (`kubectl get --raw /api/v1/namespaces/.../services/.../proxy/...`). On
// LKE-Enterprise the services/proxy subresource is webhook-denied even for the
// cluster-admin `lke-admin` ServiceAccount the health checks run as — the proxy
// fetch fails with "RBAC: access denied" despite an SSAR saying it's allowed. The
// pods/portforward subresource IS allowed, so port-forward is the portable path.
//
// withPrometheus opens ONE port-forward for the lifetime of a command's queries
// (alert-eval runs 20+), rather than a fresh kubectl per query.

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var forwardPortRe = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// forwardEstablishTimeout bounds how long we wait for kubectl to announce its
// local port — a hung kubectl (auth prompt, stuck apiserver) must not hang the
// diagnostic forever.
const forwardEstablishTimeout = 30 * time.Second

// withPrometheus opens a single kubectl port-forward to the Prometheus named by
// promSpec ("<namespace>/<service>:<port>"), invokes fn with a getter bound to
// it, and tears the forward down on return. Package var so tests can seam it.
var withPrometheus = func(promSpec string, fn func(get func(apiPath string) ([]byte, error)) error) error {
	ns, svc, port, err := parsePromSpec(promSpec)
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
	// goroutine in readForwardPortTimeout (no leak).
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	localPort, err := readForwardPortTimeout(stdout, forwardEstablishTimeout)
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
	if err := warmUpForward(client, base); err != nil {
		return err
	}
	get := func(apiPath string) ([]byte, error) {
		resp, err := client.Get(base + apiPath)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}
	return fn(get)
}

// warmUpForward retries the Prometheus readiness endpoint until the tunnel accepts
// a connection (or a short budget elapses), so the first real query doesn't race
// the port-forward coming up.
func warmUpForward(client *http.Client, base string) error {
	var lastErr error
	for i := 0; i < 15; i++ {
		resp, err := client.Get(base + "/-/ready")
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

// readForwardPortTimeout returns the local port kubectl announced, or an error if
// it doesn't announce within d. The reader goroutine unblocks when the caller
// kills the process (closing stdout).
func readForwardPortTimeout(r io.Reader, d time.Duration) (string, error) {
	type result struct {
		port string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		p, e := readForwardPort(r)
		ch <- result{p, e}
	}()
	select {
	case res := <-ch:
		return res.port, res.err
	case <-time.After(d):
		return "", fmt.Errorf("timed out after %s waiting for kubectl port-forward to start", d)
	}
}

// readForwardPort blocks until kubectl prints "Forwarding from 127.0.0.1:PORT"
// and returns PORT.
func readForwardPort(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if m := forwardPortRe.FindStringSubmatch(sc.Text()); m != nil {
			return m[1], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("kubectl port-forward did not report a local port")
}
