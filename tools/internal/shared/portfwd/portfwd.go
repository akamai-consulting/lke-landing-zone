// Package portfwd reads the local port out of `kubectl port-forward` output.
//
// IT CAME FROM internal/extensions/assertobs, where it was written for the
// Prometheus query lane and where it made internal/shared/openbao import an
// assertion extension in order to open a port-forward -- the layering inversion
// the boundary test forbids, arrived at by the usual route: a general helper
// written inside whichever capability needed it first.
//
// Parsing that one line is not an observability concern. Three unrelated callers
// need it (the Prom/Loki lanes, Keycloak configuration, and the OpenBao client's
// laptop path), which is the same "N callers of a private helper means a package"
// argument this campaign applied a dozen times inside package main.
package portfwd

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"time"
)

// ReadForwardPortTimeout returns the local port kubectl announced, or an error if
// it doesn't announce within d. The reader goroutine unblocks when the caller
// kills the process (closing stdout).
func ReadForwardPortTimeout(r io.Reader, d time.Duration) (string, error) {
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
	return "", fmt.Errorf("kubectl port-forward did not report a local port — check your kube-context/KUBECONFIG points at the target cluster and you have pods/portforward RBAC")
}

// ForwardEstablishTimeout bounds how long we wait for kubectl to announce its
// local port — a hung kubectl (auth prompt, stuck apiserver) must not hang the
// diagnostic forever.
const ForwardEstablishTimeout = 30 * time.Second

var forwardPortRe = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)
