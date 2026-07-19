package main

// Tests for the second consolidation batch:
// health-prom-rules, diagnose-argocd and fetch-kubeconfig-state.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── health-prom-rules ────────────────────────────────────────────────────────

func TestRuleEvalErrors(t *testing.T) {
	body := []byte(`{"data":{"groups":[
		{"name":"g1","rules":[{"name":"r1","lastError":""},{"name":"r2","lastError":"boom"}]},
		{"name":"g2","rules":[{"lastError":"no metric"}]}
	]}}`)
	got := ruleEvalErrors(body)
	want := []string{"g1/r2: boom", "g2/?: no metric"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ruleEvalErrors = %v, want %v", got, want)
	}
	if ruleEvalErrors([]byte("not json")) != nil {
		t.Error("bad JSON must yield no findings")
	}
}

// TestHealthPromRulesFailsClosedWhenUnreachable inverts what the old test
// asserted. It used to require a clean exit 0 when no Prometheus pod was found —
// and the pod lookup targeted llz-observability, where apl-core's Prometheus has
// never run. So the test PINNED the bug: the verb always took its skip path and
// the test called that correct.
func TestHealthPromRulesFailsClosedWhenUnreachable(t *testing.T) {
	withPrometheusStub(t, func(string, func(func(string) ([]byte, error)) error) error {
		return errors.New("no cluster")
	})
	err := runCIHealthPromRules("monitoring/prometheus-operated:9090")
	if err == nil {
		t.Fatal("an unreachable Prometheus must FAIL — a check that cannot ask has established nothing, and exit 0 reads as a green rule set")
	}
	if !strings.Contains(err.Error(), "could not query") {
		t.Errorf("error should say what it could not do: %v", err)
	}
}

func TestHealthPromRulesReportsErrors(t *testing.T) {
	withPrometheusStub(t, func(prom string, fn func(func(string) ([]byte, error)) error) error {
		// The namespace regression this fixes: the default must be apl-core's
		// Prometheus in `monitoring`, not the llz-observability namespace that holds
		// only the ServiceMonitor/PrometheusRule CRs.
		if !strings.HasPrefix(prom, "monitoring/") {
			t.Errorf("prom = %q, want it to target the monitoring namespace", prom)
		}
		return fn(func(path string) ([]byte, error) {
			if path != "/api/v1/rules" {
				return nil, errors.New("unexpected path " + path)
			}
			return []byte(`{"data":{"groups":[{"name":"g","rules":[{"name":"r","lastError":"boom"}]}]}}`), nil
		})
	})
	sum := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_STEP_SUMMARY", sum)
	t.Setenv("REGION", "primary")

	if err := runCIHealthPromRules("monitoring/prometheus-operated:9090"); err != nil {
		t.Fatalf("health-prom-rules: %v", err)
	}
	b, _ := os.ReadFile(sum)
	if !strings.Contains(string(b), "g/r: boom") {
		t.Errorf("summary missing the evaluation error:\n%s", b)
	}
}

// ── diagnose-argocd ──────────────────────────────────────────────────────────

func TestDiagnoseArgoCD(t *testing.T) {
	// Missing/empty kubeconfig → clean skip, no probes.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nope"))
	var streamed []string
	prev := diagStream
	diagStream = func(name string, args ...string) {
		streamed = append(streamed, name+" "+strings.Join(args, " "))
	}
	t.Cleanup(func() { diagStream = prev })
	if err := runCIDiagnoseArgoCD("apl-operator", "argocd"); err != nil || len(streamed) != 0 {
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
	if err := runCIDiagnoseArgoCD("apl-operator", "argocd"); err != nil {
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
	if err := runCIDiagnoseArgoCD("apl-operator", "argocd"); err != nil {
		t.Fatalf("unreachable apiserver: %v, want clean nil", err)
	}
	if len(streamed) != 0 {
		t.Errorf("unreachable apiserver should skip all diagnostic probes, streamed=%v", streamed)
	}
}

// ── fetch-kubeconfig-state ───────────────────────────────────────────────────

func TestFetchKubeconfigState(t *testing.T) {
	t.Setenv("TF_STATE_BUCKET", "tf-state")
	outPath := filepath.Join(t.TempDir(), "kube", "config")

	var initArgs []string
	prevInit := tfInitStream
	tfInitStream = func(args ...string) error { initArgs = args; return nil }
	t.Cleanup(func() { tfInitStream = prevInit })

	// The output extraction runs `terraform output -raw kubeconfig_raw` via
	// os/exec directly; stub the terraform binary on PATH with a script.
	binDir := t.TempDir()
	fake := "#!/bin/sh\nif [ \"$3\" = kubeconfig_raw ]; then printf 'apiVersion: v1'; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, "terraform"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := runCIFetchKubeconfigState("primary", outPath, false); err != nil {
		t.Fatalf("fetch-kubeconfig-state: %v", err)
	}
	if got := strings.Join(initArgs, " "); !strings.Contains(got, "key=cluster/primary/terraform.tfstate") ||
		!strings.Contains(got, "bucket=tf-state") {
		t.Errorf("init backend config = %q", got)
	}
	b, err := os.ReadFile(outPath)
	if err != nil || string(b) != "apiVersion: v1" {
		t.Errorf("kubeconfig file = %q (%v)", b, err)
	}
	if st, _ := os.Stat(outPath); st.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig mode = %v, want 0600", st.Mode().Perm())
	}

	// Empty output: --allow-missing reports available=false; without it, error.
	empty := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "terraform"), []byte(empty), 0o755); err != nil {
		t.Fatal(err)
	}
	ghOut := filepath.Join(t.TempDir(), "out")
	t.Setenv("GITHUB_OUTPUT", ghOut)
	if err := runCIFetchKubeconfigState("primary", outPath, true); err != nil {
		t.Errorf("allow-missing must not fail: %v", err)
	}
	if b, _ := os.ReadFile(ghOut); !strings.Contains(string(b), "available=false") {
		t.Errorf("GITHUB_OUTPUT = %q, want available=false", b)
	}
	if err := runCIFetchKubeconfigState("primary", outPath, false); err == nil {
		t.Error("empty kubeconfig_raw without allow-missing must fail")
	}

	// Failed init aborts before any output read — and is retried before giving up.
	prevSleep := tfInitSleep
	var initSleeps int
	tfInitSleep = func(time.Duration) { initSleeps++ }
	t.Cleanup(func() { tfInitSleep = prevSleep })
	var initCalls int
	tfInitStream = func(...string) error { initCalls++; return errors.New("backend error") }
	if err := runCIFetchKubeconfigState("primary", outPath, false); err == nil ||
		!strings.Contains(err.Error(), "terraform init failed") {
		t.Errorf("init failure: err = %v", err)
	}
	if initCalls != tfInitAttempts || initSleeps != tfInitAttempts-1 {
		t.Errorf("init tried %d times with %d sleeps, want %d/%d", initCalls, initSleeps, tfInitAttempts, tfInitAttempts-1)
	}
}

// TestHealthPromRulesRefusesVacuousGreen covers the two ways this reported
// health it had not observed. promRulesJSON had no Status field, so an
// {"status":"error"} envelope unmarshalled cleanly with zero groups — and zero
// groups then read as "no evaluation errors", which is an affirmative claim
// derived from a failure. Zero groups is also the ruleSelector regression
// (a PrometheusRule missing `prometheus: system` is never LOADED, so it
// evaluates nothing and reports nothing) that monitoring-label-guard exists for.
func TestHealthPromRulesRefusesVacuousGreen(t *testing.T) {
	tests := []struct {
		name, body, wantErr string
	}{
		{
			name:    "prometheus error envelope at HTTP 200",
			body:    `{"status":"error","error":"query engine unavailable"}`,
			wantErr: "query engine unavailable",
		},
		{
			name:    "zero rule groups loaded",
			body:    `{"status":"success","data":{"groups":[]}}`,
			wantErr: "ZERO rule groups",
		},
		{
			name:    "unparseable body",
			body:    `not json`,
			wantErr: "could not parse",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withPrometheusStub(t, func(_ string, fn func(func(string) ([]byte, error)) error) error {
				return fn(func(string) ([]byte, error) { return []byte(tt.body), nil })
			})
			t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "sum"))
			t.Setenv("REGION", "primary")
			err := runCIHealthPromRules("monitoring/prometheus-operated:9090")
			if err == nil {
				t.Fatalf("must fail rather than report healthy rules it never observed (body: %s)", tt.body)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}

	// A real, loaded rule set still passes.
	t.Run("loaded groups with no lastError pass", func(t *testing.T) {
		withPrometheusStub(t, func(_ string, fn func(func(string) ([]byte, error)) error) error {
			return fn(func(string) ([]byte, error) {
				return []byte(`{"status":"success","data":{"groups":[{"name":"g","rules":[{"name":"r"}]}]}}`), nil
			})
		})
		t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(t.TempDir(), "sum"))
		t.Setenv("REGION", "primary")
		if err := runCIHealthPromRules("monitoring/prometheus-operated:9090"); err != nil {
			t.Errorf("a loaded rule set with no errors must pass, got %v", err)
		}
	})
}
