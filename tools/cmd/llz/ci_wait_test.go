package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWaitPoll(t *testing.T) {
	// Succeeds on the 3rd try.
	n := 0
	if !waitPoll(time.Second, time.Millisecond, func() bool { n++; return n == 3 }) || n != 3 {
		t.Errorf("waitPoll succeeded=%v after %d tries, want true at 3", n == 3, n)
	}
	// A zero/negative budget still gets exactly one immediate try.
	n = 0
	if waitPoll(0, time.Millisecond, func() bool { n++; return false }) {
		t.Error("waitPoll should be false when cond never holds")
	}
	if n != 1 {
		t.Errorf("waitPoll tried %d times under a zero budget, want 1", n)
	}
	if !waitPoll(0, time.Millisecond, func() bool { return true }) {
		t.Error("waitPoll should succeed on an immediate true even with a zero budget")
	}
}

func TestRunCIWaitPods(t *testing.T) {
	// Both pods Running → 0, no diagnostics fetched.
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.HasPrefix(a, "-n ns get pod ") && strings.HasSuffix(a, "-o jsonpath={.status.phase}") {
			return []byte("Running"), nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	if got := runCIWaitPods("ns", "Running", []string{"p-0", "p-1"}, 0, 0); got != 0 {
		t.Errorf("wait-pods (all Running) = %d, want 0", got)
	}

	// Pod stuck Pending → 1, and the timeout path dumps diagnostics via the
	// combined-output seam: workloads, the stuck pod's describe, and events.
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.HasSuffix(a, "jsonpath={.status.phase}") {
			return []byte("Pending"), nil
		}
		return nil, errors.New("unexpected execOutput: " + a)
	})
	var sawWorkloads, sawDescribe, sawEvents bool
	origCombined := execCombined
	execCombined = func(name string, args ...string) string {
		a := strings.Join(args, " ")
		switch {
		case name == "kubectl" && strings.Contains(a, "get statefulset,pod"):
			sawWorkloads = true
		case name == "kubectl" && strings.HasPrefix(a, "-n ns describe pod p-0"):
			sawDescribe = true
		case name == "kubectl" && strings.Contains(a, "get events"):
			sawEvents = true
		}
		return "diag\n"
	}
	t.Cleanup(func() { execCombined = origCombined })
	if got := runCIWaitPods("ns", "Running", []string{"p-0"}, 0, 0); got != 1 {
		t.Errorf("wait-pods (stuck Pending) = %d, want 1", got)
	}
	if !sawWorkloads || !sawDescribe || !sawEvents {
		t.Errorf("timeout diagnostics: workloads=%v describe=%v events=%v, want all",
			sawWorkloads, sawDescribe, sawEvents)
	}

	// Missing --namespace is an immediate usage failure.
	if got := runCIWaitPods("", "Running", []string{"p-0"}, 0, 0); got != 1 {
		t.Errorf("wait-pods (no namespace) = %d, want 1", got)
	}
}

func TestRunCIWaitSecret(t *testing.T) {
	stubWait := func(t *testing.T, err error) *[]string {
		t.Helper()
		var calls []string
		orig := kubectlWaitStream
		kubectlWaitStream = func(args ...string) error {
			calls = append(calls, strings.Join(args, " "))
			return err
		}
		t.Cleanup(func() { kubectlWaitStream = orig })
		return &calls
	}

	// Secret present + ExternalSecret goes Ready → 0.
	withKubectl(t, func(a string) ([]byte, error) {
		if a == "-n cert-manager get secret tok" {
			return nil, nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	calls := stubWait(t, nil)
	if got := runCIWaitSecret("cert-manager", "tok", "tok-es", 0, 0, 60); got != 0 {
		t.Errorf("wait-secret (present, ES Ready) = %d, want 0", got)
	}
	if len(*calls) != 1 || !strings.Contains((*calls)[0], "externalsecret/tok-es") {
		t.Errorf("kubectl wait calls = %v, want one for externalsecret/tok-es", *calls)
	}

	// Secret present but the ExternalSecret never goes Ready → 1.
	withKubectl(t, func(a string) ([]byte, error) { return nil, nil })
	stubWait(t, errors.New("timed out"))
	if got := runCIWaitSecret("cert-manager", "tok", "tok-es", 0, 0, 60); got != 1 {
		t.Errorf("wait-secret (ES never Ready) = %d, want 1", got)
	}

	// Secret never appears → 1, conditions dumped from the ExternalSecret.
	var sawConditions bool
	withKubectl(t, func(a string) ([]byte, error) {
		if a == "-n cert-manager get externalsecret tok-es -o jsonpath={.status.conditions}" {
			sawConditions = true
			return []byte(`[{"type":"Ready","status":"False"}]`), nil
		}
		return nil, errors.New("nope")
	})
	calls = stubWait(t, nil)
	if got := runCIWaitSecret("cert-manager", "tok", "tok-es", 0, 0, 60); got != 1 {
		t.Errorf("wait-secret (absent) = %d, want 1", got)
	}
	if !sawConditions {
		t.Error("timeout path should dump the ExternalSecret conditions")
	}
	if len(*calls) != 0 {
		t.Errorf("kubectl wait should not run when the Secret never appears, got %v", *calls)
	}

	// No --externalsecret: existence alone is success, no condition wait.
	withKubectl(t, func(a string) ([]byte, error) { return nil, nil })
	calls = stubWait(t, errors.New("must not be called"))
	if got := runCIWaitSecret("ns", "s", "", 0, 0, 60); got != 0 {
		t.Errorf("wait-secret (no ES) = %d, want 0", got)
	}
	if len(*calls) != 0 {
		t.Errorf("kubectl wait should not run without --externalsecret, got %v", *calls)
	}

	// Missing flags are an immediate usage failure.
	if got := runCIWaitSecret("", "", "", 0, 0, 60); got != 1 {
		t.Errorf("wait-secret (no flags) = %d, want 1", got)
	}
}

func TestRunCIWaitClusterReady(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		if a == "get nodes --request-timeout=10s" {
			return []byte("NAME STATUS\nnode-1 Ready\n"), nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	if got := runCIWaitClusterReady(0, 0, 10); got != 0 {
		t.Errorf("wait-cluster-ready (reachable) = %d, want 0", got)
	}

	// Unreachable: exit 1, and the timeout path probes the apiserver directly
	// (an answering /version implicates the kubeconfig/ACL, not provisioning).
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			t.Errorf("probed %q, want /version", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer probe.Close()
	withKubectl(t, func(a string) ([]byte, error) {
		if a == "config view --minify -o jsonpath={.clusters[0].cluster.server}" {
			return []byte(probe.URL), nil
		}
		return nil, errors.New("connection refused")
	})
	if got := runCIWaitClusterReady(0, 0, 10); got != 1 {
		t.Errorf("wait-cluster-ready (unreachable) = %d, want 1", got)
	}
}
