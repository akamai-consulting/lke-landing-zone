package recondiag

// diagnose_test.go — what this verb COLLECTS is its entire value, so that is what
// is pinned. A diagnostic that quietly stops gathering the one artifact it was
// written for is worse than none: it makes the bundle look complete.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// TestProbesCollectWhatConvergeTellsYouToCheck is the coupling to the gate
// message. converge says "check llz-reconciler llz_apl_overlay_synced" when the
// obj chain stalls; before this verb, nothing in the failure bundle contained the
// reconciler's logs, that gauge, or apl-core's effective obj settings — so the
// instruction pointed into a cluster that was already gone.
func TestProbesCollectWhatConvergeTellsYouToCheck(t *testing.T) {
	var all strings.Builder
	for _, p := range Probes(Namespace) {
		all.WriteString(strings.Join(p.Args, " "))
		all.WriteString("\n")
	}
	got := all.String()

	for _, want := range []struct{ needle, why string }{
		{"logs deploy/llz-reconciler", "the reconciler's own logs — every apl-overlay skip prints there, and that line is the difference between settling and stalled"},
		{"--previous", "the crashed container, when the lane crash-loops"},
		{"get cm loki", "apl-core's rendered Loki config — the in-cluster view of whether obj converged (AplObjectStorage is a git FILE, not a CRD: kubectl cannot see it)"},
		{"loki-s3-linode-credentials", "whether ESO ever built the credential the chain exists to produce"},
		{"lease", "leader election — a lane that never acquires never writes"},
	} {
		if !strings.Contains(got, want.needle) {
			t.Errorf("probes must collect %q — %s\ngot:\n%s", want.needle, want.why, got)
		}
	}
}

// TestProbesHonourTheNamespaceFlag — a diagnostic pointed at the wrong namespace
// returns "No resources found", which reads exactly like a healthy absence.
func TestProbesHonourTheNamespaceFlag(t *testing.T) {
	for _, p := range Probes("custom-ns") {
		for i, a := range p.Args {
			if a == "-n" && i+1 < len(p.Args) && p.Args[i+1] == Namespace {
				t.Errorf("probe %q ignored the namespace flag and hardcoded %q", p.Why, Namespace)
			}
		}
	}
}

// TestEveryProbeExplainsItself — the Why prints as the ::group:: header, so a
// reader scanning a long bundle can tell what each block is for. An unlabelled
// dump is why these bundles go unread.
func TestEveryProbeExplainsItself(t *testing.T) {
	ps := Probes(Namespace)
	if len(ps) == 0 {
		t.Fatal("no probes — the verb would produce an empty bundle and exit 0, which is the failure mode it exists to prevent")
	}
	for _, p := range ps {
		if strings.TrimSpace(p.Why) == "" {
			t.Errorf("probe %v has no Why", p.Args)
		}
		if len(p.Args) < 2 {
			t.Errorf("probe %q has no command", p.Why)
		}
	}
}

// TestRunSkipsWithoutAKubeconfig — the cluster may not exist. Skipping must be
// silent-and-zero, never a pile of 30s dial timeouts that eat the job budget.
func TestRunSkipsWithoutAKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", t.TempDir()) // so ~/.kube/config cannot be picked up

	called := 0
	orig := diagStream
	diagStream = func(string, ...string) { called++ }
	t.Cleanup(func() { diagStream = orig })

	if err := Run(Namespace); err != nil {
		t.Fatalf("diagnostics must never fail: %v", err)
	}
	if called != 0 {
		t.Errorf("ran %d probe(s) with no kubeconfig — each would block on its own dial timeout", called)
	}
}

// TestRunStreamsEveryProbeWhenReachable covers the path that matters: with a
// cluster in front of it, every probe in the plan actually runs. A verb that
// silently ran a SUBSET would produce a bundle that looks complete and is not —
// the same shape as the gap it exists to close.
func TestRunStreamsEveryProbeWhenReachable(t *testing.T) {
	kube := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kube, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kube)

	prevExec := kubectlprobe.Exec
	kubectlprobe.Exec = func(string, ...string) ([]byte, error) { return []byte("Client Version: v1.31.0"), nil }
	t.Cleanup(func() { kubectlprobe.Exec = prevExec })

	var ran [][]string
	prevStream := diagStream
	diagStream = func(name string, args ...string) { ran = append(ran, append([]string{name}, args...)) }
	t.Cleanup(func() { diagStream = prevStream })

	if err := Run(Namespace); err != nil {
		t.Fatalf("diagnostics must never fail: %v", err)
	}
	if want := len(Probes(Namespace)); len(ran) != want {
		t.Fatalf("ran %d probe(s), plan has %d — a partial bundle reads as a complete one", len(ran), want)
	}
}

// TestRunSkipsWhenTheAPIServerIsUnreachable — the ACL-not-granted case. Each probe
// would otherwise block on its own ~30s dial timeout; argodiag's comment records
// that pile-up eating a ~49m job before the run was force-cancelled.
func TestRunSkipsWhenTheAPIServerIsUnreachable(t *testing.T) {
	kube := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kube, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kube)

	prevExec := kubectlprobe.Exec
	kubectlprobe.Exec = func(string, ...string) ([]byte, error) { return nil, errors.New("dial tcp: i/o timeout") }
	t.Cleanup(func() { kubectlprobe.Exec = prevExec })

	called := 0
	prevStream := diagStream
	diagStream = func(string, ...string) { called++ }
	t.Cleanup(func() { diagStream = prevStream })

	if err := Run(Namespace); err != nil {
		t.Fatalf("diagnostics must never fail: %v", err)
	}
	if called != 0 {
		t.Errorf("ran %d probe(s) against an unreachable apiserver", called)
	}
}
