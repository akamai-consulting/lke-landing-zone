package main

// Tests for the second consolidation batch: stash-env-secret,
// health-prom-rules, diagnose-argocd and fetch-kubeconfig-state.

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── stash-env-secret ─────────────────────────────────────────────────────────

// stubGHSecretSet records env-scoped gh secret writes, failing those named in
// failFor.
func stubGHSecretSet(t *testing.T, failFor map[string]bool) *[]string {
	t.Helper()
	var calls []string
	prev := ghSetSecretFn
	ghSetSecretFn = func(name, ghEnv, value string) error {
		calls = append(calls, fmt.Sprintf("%s@%s=%s", name, ghEnv, value))
		if failFor[name] {
			return errors.New("HTTP 403")
		}
		return nil
	}
	t.Cleanup(func() { ghSetSecretFn = prev })
	return &calls
}

func TestStashEnvSecret(t *testing.T) {
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name == "terraform" && len(args) == 3 && args[1] == "-raw" {
			switch args[2] {
			case "loki_access_key":
				return []byte("AKIA1\n"), nil
			case "loki_secret_key":
				return []byte("shh\n"), nil
			case "empty_output":
				return []byte(""), nil
			}
		}
		return nil, errors.New("unexpected: " + name + " " + strings.Join(args, " "))
	})

	// Both mappings stored.
	calls := stubGHSecretSet(t, nil)
	err := runCIStashEnvSecret("infra-primary", []string{
		"loki_access_key=LOKI_S3_ACCESS_KEY", "loki_secret_key=LOKI_S3_SECRET_KEY"})
	if err != nil {
		t.Fatalf("stash: %v", err)
	}
	want := []string{"LOKI_S3_ACCESS_KEY@infra-primary=AKIA1", "LOKI_S3_SECRET_KEY@infra-primary=shh"}
	if strings.Join(*calls, " ") != strings.Join(want, " ") {
		t.Errorf("writes = %v, want %v", *calls, want)
	}

	// An empty terraform output fails — after attempting the rest.
	calls = stubGHSecretSet(t, nil)
	err = runCIStashEnvSecret("infra-primary", []string{
		"empty_output=BROKEN", "loki_access_key=LOKI_S3_ACCESS_KEY"})
	if err == nil {
		t.Error("empty output must fail the stash")
	}
	if len(*calls) != 1 {
		t.Errorf("remaining mappings must still be attempted, got %v", *calls)
	}

	// A failed gh write fails the run but does not abort the list.
	calls = stubGHSecretSet(t, map[string]bool{"LOKI_S3_ACCESS_KEY": true})
	err = runCIStashEnvSecret("infra-primary", []string{
		"loki_access_key=LOKI_S3_ACCESS_KEY", "loki_secret_key=LOKI_S3_SECRET_KEY"})
	if err == nil || len(*calls) != 2 {
		t.Errorf("partial gh failure: err=%v calls=%v, want error after both attempts", err, *calls)
	}

	// Usage errors.
	if err := runCIStashEnvSecret("", []string{"a=B"}); err == nil {
		t.Error("missing --env must fail")
	}
	if err := runCIStashEnvSecret("infra-primary", []string{"malformed"}); err == nil {
		t.Error("malformed mapping must fail")
	}
}

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

func TestHealthPromRulesSkipsWithoutPod(t *testing.T) {
	withKubectl(t, func(string) ([]byte, error) { return []byte(""), nil })
	if err := runCIHealthPromRules(19090, 1); err != nil {
		t.Errorf("no Prometheus pod must skip cleanly: %v", err)
	}
}

func TestHealthPromRulesReportsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/-/ready":
			w.WriteHeader(200)
		case "/api/v1/rules":
			fmt.Fprint(w, `{"data":{"groups":[{"name":"g","rules":[{"name":"r","lastError":"boom"}]}]}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	var port int
	if _, err := fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port); err != nil {
		t.Skipf("could not parse httptest port from %s", srv.URL)
	}
	withKubectl(t, func(a string) ([]byte, error) {
		if strings.Contains(a, "get pod") {
			return []byte("prometheus-0"), nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	prev := startAttachedPortForward
	startAttachedPortForward = func(ns, target, ports string) (func(), error) {
		if ns != "llz-observability" || target != "prometheus-0" {
			t.Errorf("port-forward %s/%s, want llz-observability/prometheus-0", ns, target)
		}
		return func() {}, nil
	}
	t.Cleanup(func() { startAttachedPortForward = prev })
	sum := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_STEP_SUMMARY", sum)
	t.Setenv("REGION", "primary")

	if err := runCIHealthPromRules(port, 2); err != nil {
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
		case a == "-n argocd get pods -o name":
			return []byte("pod/argocd-server-0\n"), nil
		case a == "-n argocd get jobs -o name":
			return []byte("job.batch/hook-1\n"), nil
		case a == "-n apl-operator get pods -o name":
			return []byte("pod/apl-0\n"), nil
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
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("probes missing %q:\n%s", want, joined)
		}
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

	// Failed init aborts before any output read.
	tfInitStream = func(...string) error { return errors.New("backend error") }
	if err := runCIFetchKubeconfigState("primary", outPath, false); err == nil ||
		!strings.Contains(err.Error(), "terraform init failed") {
		t.Errorf("init failure: err = %v", err)
	}
}
