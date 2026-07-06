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

// promGet fetches apiPath (e.g. "/api/v1/label/__name__/values" or
// "/api/v1/query?query=up") from the Prometheus named by promSpec
// ("<namespace>/<service>:<port>"), through an ephemeral kubectl port-forward.
// Package var so tests can seam it. Returns the raw response body.
var promGet = func(promSpec, apiPath string) ([]byte, error) {
	ns, svc, port, err := parsePromSpec(promSpec)
	if err != nil {
		return nil, err
	}
	// Local port ":0" → kubectl picks a free port and announces it on stdout.
	cmd := exec.Command("kubectl", "port-forward", "-n", ns, "svc/"+svc, ":"+port)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("kubectl port-forward: %w", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	localPort, err := readForwardPort(stdout)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%s%s", localPort, apiPath)
	client := &http.Client{Timeout: 15 * time.Second}
	// The forward is announced ready, but the tunnel can need a beat to accept the
	// first connection — retry a few times on connection error.
	var lastErr error
	for i := 0; i < 10; i++ {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		return body, rerr
	}
	return nil, fmt.Errorf("GET %s via port-forward: %w", apiPath, lastErr)
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
