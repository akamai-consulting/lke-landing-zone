package argodiag

// moved_test.go — tests that were stranded in package main by their FILENAME.
//
// TestDiagnoseArgoCD lived in `ci_batch2_test.go`. That is the THIRD naming
// pattern this tree has found stranding tests, after files named for a
// coverage METRIC (coverage_tier1/2, branch_coverage, uncovered_helpers) and
// files named for the COMMAND that calls the code (env_set_test.go, which held
// zero tests for env_set.go). This one is named for the BATCH it was written in —
// a fact about the day's work, not about the code — so nothing about the name
// suggests where the test belongs, and it took a build failure to find it.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnoseArgoCD(t *testing.T) {
	// Missing/empty kubeconfig → clean skip, no probes.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nope"))
	var streamed []string
	prev := diagStream
	diagStream = func(name string, args ...string) {
		streamed = append(streamed, name+" "+strings.Join(args, " "))
	}
	t.Cleanup(func() { diagStream = prev })
	if err := Run("apl-operator", "argocd"); err != nil || len(streamed) != 0 {
		t.Fatalf("missing kubeconfig: err=%v streamed=%v, want clean skip", err, streamed)
	}

	// With a kubeconfig: probes run, per-pod describes and job logs included,
	// both namespaces swept (apl-operator + argocd), and the command still
	// never errors.
	kc := filepath.Join(t.TempDir(), "kc")
	if err := os.WriteFile(kc, []byte("apiVersion: v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kc)
	withKubectl(t, func(a string) ([]byte, error) {
		switch {
		// Reachability gate: apiserver answers, so diagnostics proceed.
		case a == "version --request-timeout=10s":
			return nil, nil
		case a == "-n argocd get pods -o name":
			return []byte("pod/argocd-server-0\n"), nil
		case a == "-n argocd get jobs -o name":
			return []byte("job.batch/hook-1\n"), nil
		case a == "-n apl-operator get pods -o name":
			return []byte("pod/apl-0\n"), nil
		// All-namespace failing-workload sweep: one crashlooping pod + one failed Job.
		case a == "get pods -A -o json":
			return items(
				`{"metadata":{"namespace":"otomi","name":"otomi-api-x"},"status":{"phase":"Running","containerStatuses":[{"name":"otomi-api","ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}},{"name":"tools","ready":true,"state":{"running":{}}}]}}`,
				`{"metadata":{"namespace":"x","name":"healthy"},"status":{"phase":"Running","containerStatuses":[{"name":"c","ready":true,"state":{"running":{}}}]}}`,
			), nil
		case a == "get jobs -A -o json":
			return items(`{"metadata":{"namespace":"harbor","name":"harbor-robot-provisioner-123"},"status":{"failed":2}}`), nil
		}
		return nil, errors.New("best-effort")
	})
	if err := Run("apl-operator", "argocd"); err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	joined := strings.Join(streamed, "\n")
	for _, want := range []string{
		"kubectl get nodes -o wide",
		// apl-operator swept first — the likely fresh-cluster failure point.
		"kubectl get all -n apl-operator -o wide",
		"kubectl describe -n apl-operator pod/apl-0",
		"helm history apl -n apl-operator",
		// argocd still swept too.
		"kubectl describe -n argocd pod/argocd-server-0",
		"kubectl logs -n argocd job.batch/hook-1 --all-containers --tail=200",
		"helm history argocd -n argocd",
		// Convergence-blocker capture: Argo Application states + the phase1
		// platform-app-ca CA chain.
		"kubectl -n argocd get applications -o custom-columns=NAME:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status,MESSAGE:.status.conditions[*].message",
		"kubectl -n argocd get application platform-bootstrap -o yaml",
		"kubectl -n cert-manager get secret platform-app-ca -o wide",
		"kubectl get certificate,certificaterequest --all-namespaces -o wide",
		"kubectl get clusterissuer -o wide",
		// Failing-workload sweep: describe + previous/current logs for the
		// crashlooping pod's containers, and logs for the failed Job.
		"kubectl -n otomi describe pod otomi-api-x",
		"kubectl -n otomi logs otomi-api-x -c otomi-api --previous --tail=60",
		"kubectl -n otomi logs otomi-api-x -c otomi-api --tail=40",
		"kubectl -n harbor logs job/harbor-robot-provisioner-123 --all-containers --tail=120",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("probes missing %q:\n%s", want, joined)
		}
	}
	// The healthy pod must NOT be probed.
	if strings.Contains(joined, "describe pod healthy") {
		t.Error("healthy pod should not be swept")
	}

	// Kubeconfig present but apiserver unreachable (runner never allowlisted on the
	// control-plane firewall): the reachability gate must bail after the single
	// bounded probe, before any of the unbounded sweeps — otherwise each one blocks
	// on its ~30s dial timeout and the pile-up burns the whole job budget.
	streamed = nil
	withKubectl(t, func(a string) ([]byte, error) {
		return nil, errors.New("dial tcp: i/o timeout") // every call fails, incl. the version probe
	})
	if err := Run("apl-operator", "argocd"); err != nil {
		t.Fatalf("unreachable apiserver: %v, want clean nil", err)
	}
	if len(streamed) != 0 {
		t.Errorf("unreachable apiserver should skip all diagnostic probes, streamed=%v", streamed)
	}
}

// TestHealthPromRulesRefusesVacuousGreen covers the two ways this reported
// health it had not observed. promRulesJSON had no Status field, so an
// {"status":"error"} envelope unmarshalled cleanly with zero groups — and zero
// groups then read as "no evaluation errors", which is an affirmative claim
// derived from a failure. Zero groups is also the ruleSelector regression
// (a PrometheusRule missing `prometheus: system` is never LOADED, so it
// evaluates nothing and reports nothing) that monitoring-label-guard exists for.
