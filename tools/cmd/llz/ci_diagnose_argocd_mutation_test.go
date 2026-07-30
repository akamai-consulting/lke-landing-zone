package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// withDiagStream swaps the streaming-probe seam so the diagnostics never shell
// out to a real kubectl/helm, and records the probe sequence.
func withDiagStream(t *testing.T) *[]string {
	t.Helper()
	orig := diagStream
	calls := new([]string)
	diagStream = func(name string, args ...string) {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
	}
	t.Cleanup(func() { diagStream = orig })
	return calls
}

// The node sweep prints the scheduling-relevant lines it CAPTURED — the whole
// point of switching from a piped `describe | grep` to a captured describe. A
// successful describe must be parsed, not discarded.
func TestDiagnoseArgoCDPrintsCapturedNodeSchedulingLines(t *testing.T) {
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	writeFile(t, kc, "apiVersion: v1\nclusters: []\n")
	t.Setenv("KUBECONFIG", kc)
	withDiagStream(t)

	const describe = "Name:               lke-node-1\n" +
		"Roles:              <none>\n" +
		"Taints:             node.kubernetes.io/not-ready:NoSchedule\n" +
		"Conditions:\n" +
		"  Ready   False\n" +
		"Allocated resources:\n" +
		"  cpu 100m\n"
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			return nil, errors.New("unexpected command " + name)
		}
		switch a := strings.Join(args, " "); {
		case strings.HasPrefix(a, "version"):
			return []byte("Client Version: v1.31.0\n"), nil // apiserver reachable
		case a == "describe nodes":
			return []byte(describe), nil
		}
		return nil, errors.New("probe not stubbed (best-effort)")
	})

	out := captureStdout(t, func() {
		if err := runCIDiagnoseArgoCD("apl-operator", "argocd"); err != nil {
			t.Fatalf("diagnostics must never fail: %v", err)
		}
	})
	for _, want := range []string{
		"Name:               lke-node-1",
		"Taints:             node.kubernetes.io/not-ready:NoSchedule",
		"Conditions:",
		"Allocated resources:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("node sweep dropped %q from a successful describe:\n%s", want, out)
		}
	}
	// Only the scheduling-relevant sections are echoed.
	if strings.Contains(out, "Roles:") {
		t.Errorf("node sweep printed a non-scheduling line:\n%s", out)
	}
}

// Events are the highest-signal capture in the per-namespace sweep
// (FailedScheduling / ImagePull / PVC binding all land there), so a successful
// `get events` must be printed, not swallowed.
func TestDiagnoseNamespacePrintsRecentEvents(t *testing.T) {
	withDiagStream(t)
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "get events") {
			return []byte("2m  Warning  FailedScheduling  pod/apl-operator-0  no nodes available\n"), nil
		}
		return nil, errors.New("probe not stubbed (best-effort)")
	})

	out := captureStdout(t, func() { diagnoseNamespace("apl-operator", "apl") })
	if !strings.Contains(out, "FailedScheduling") {
		t.Errorf("a successful events read must be printed:\n%s", out)
	}
}
