package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// stubKubectlWait replaces kubectlWaitStream with a recorder whose per-call
// result is decided by fn (keyed off the joined args), restoring it on cleanup.
func stubKubectlWait(t *testing.T, fn func(args string) error) *[]string {
	t.Helper()
	var calls []string
	orig := kubectlWaitStream
	kubectlWaitStream = func(args ...string) error {
		a := strings.Join(args, " ")
		calls = append(calls, a)
		return fn(a)
	}
	t.Cleanup(func() { kubectlWaitStream = orig })
	return &calls
}

func TestRunCIWaitPods(t *testing.T) {
	// Both pods reach the phase → 0, no diagnostics fetched. Each pod is a
	// create-wait then a phase-wait, so two pods = four kubectl wait calls.
	calls := stubKubectlWait(t, func(string) error { return nil })
	if got := runCIWaitPods("ns", "Running", []string{"p-0", "p-1"}, 0, 0); got != 0 {
		t.Errorf("wait-pods (all Running) = %d, want 0", got)
	}
	if len(*calls) != 4 {
		t.Errorf("wait-pods (2 pods) made %d kubectl wait calls, want 4: %v", len(*calls), *calls)
	}

	// Pod stuck (phase wait times out) → 1, and the timeout path dumps diagnostics
	// via the combined-output seam: workloads, the stuck pod's describe, and events.
	stubKubectlWait(t, func(a string) error {
		if strings.Contains(a, "jsonpath") {
			return errors.New("timed out waiting for phase")
		}
		return nil
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

func TestCountReadyNodes(t *testing.T) {
	for _, c := range []struct {
		name, out string
		want      int
	}{
		{"empty (reachable but no nodes)", "", 0},
		{"all ready", "node-1=True\nnode-2=True\nnode-3=True\n", 3},
		{"mixed", "node-1=True\nnode-2=False\nnode-3=Unknown\n", 1},
		{"condition absent", "node-1=\nnode-2=True\n", 1},
		{"trailing whitespace", "  node-1=True  \n\n", 1},
	} {
		if got := countReadyNodes(c.out); got != c.want {
			t.Errorf("%s: countReadyNodes = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestResolveExpectNodes(t *testing.T) {
	// No tfvars → fallback, floored at 1.
	if got := resolveExpectNodes("", 3); got != 3 {
		t.Errorf("resolveExpectNodes(\"\", 3) = %d, want 3", got)
	}
	if got := resolveExpectNodes("", 0); got != 1 {
		t.Errorf("resolveExpectNodes(\"\", 0) = %d, want 1 (floored)", got)
	}
	// Unreadable path → fallback.
	if got := resolveExpectNodes(filepath.Join(t.TempDir(), "nope.tfvars"), 2); got != 2 {
		t.Errorf("resolveExpectNodes(missing, 2) = %d, want 2", got)
	}
	// tfvars node_count wins over the flag fallback.
	f := filepath.Join(t.TempDir(), "e2e.tfvars")
	if err := os.WriteFile(f, []byte("node_count = 5\nnode_type = \"g8-dedicated-8-4\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveExpectNodes(f, 1); got != 5 {
		t.Errorf("resolveExpectNodes(node_count=5, 1) = %d, want 5", got)
	}
	// node_count absent in the file → fallback.
	f2 := filepath.Join(t.TempDir(), "no-count.tfvars")
	if err := os.WriteFile(f2, []byte("node_type = \"g8-dedicated-8-4\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveExpectNodes(f2, 2); got != 2 {
		t.Errorf("resolveExpectNodes(no node_count, 2) = %d, want 2", got)
	}
}

func TestRunCIWaitClusterReady(t *testing.T) {
	// node-readiness jsonpath probe → "<node>=<Ready status>" per line.
	const nodesArg = "get nodes -o "
	origCombined := execCombined
	execCombined = func(string, ...string) string { return "" }
	t.Cleanup(func() { execCombined = origCombined })

	// Reachable AND the expected 2 nodes Ready → 0.
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.HasPrefix(a, nodesArg) {
			return []byte("node-1=True\nnode-2=True\n"), nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	if got := runCIWaitClusterReady(0, 0, 10, 2); got != 0 {
		t.Errorf("wait-cluster-ready (2 Ready, expect 2) = %d, want 0", got)
	}

	// Reachable but only 1 of the expected 3 nodes Ready → 1 (pool never came up).
	// diagnoseAPIServer's config-view returns nothing here, exercising its
	// best-effort "unknown endpoint" path.
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.HasPrefix(a, nodesArg) {
			return []byte("node-1=True\n"), nil
		}
		return nil, errors.New("no server")
	})
	if got := runCIWaitClusterReady(0, 0, 10, 3); got != 1 {
		t.Errorf("wait-cluster-ready (1 Ready, expect 3) = %d, want 1", got)
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
	if got := runCIWaitClusterReady(0, 0, 10, 1); got != 1 {
		t.Errorf("wait-cluster-ready (unreachable) = %d, want 1", got)
	}
}
