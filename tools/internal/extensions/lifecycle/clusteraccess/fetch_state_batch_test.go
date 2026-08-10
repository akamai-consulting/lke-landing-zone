package clusteraccess

// fetch_state_batch_test.go — moved from package main's ci_batch2_test.go,
// a file that grouped tests by when they were written rather than by what they
// test. It exercises the state-read path end to end, so it belongs with the code.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── fetch-kubeconfig-state ───────────────────────────────────────────────────

func TestFetchKubeconfigState(t *testing.T) {
	d := testDeps(t)
	t.Setenv("TF_STATE_BUCKET", "tf-state")
	outPath := filepath.Join(t.TempDir(), "kube", "config")

	var initArgs []string
	prevInit := TfInitStream
	TfInitStream = func(args ...string) error { initArgs = args; return nil }
	t.Cleanup(func() { TfInitStream = prevInit })

	// The output extraction runs `tofu output -raw kubeconfig_raw` via os/exec
	// (through tfCommand); stub that binary on PATH with a script. The name must
	// track tfbin.Bin()'s preference — a stub called `terraform` would simply be
	// ignored in favour of a real tofu on the developer's PATH.
	binDir := t.TempDir()
	fake := "#!/bin/sh\nif [ \"$3\" = kubeconfig_raw ]; then printf 'apiVersion: v1'; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, "tofu"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := RunFetchFromState(d, "primary", outPath, false); err != nil {
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
	if err := os.WriteFile(filepath.Join(binDir, "tofu"), []byte(empty), 0o755); err != nil {
		t.Fatal(err)
	}
	ghOut := filepath.Join(t.TempDir(), "out")
	t.Setenv("GITHUB_OUTPUT", ghOut)
	if err := RunFetchFromState(d, "primary", outPath, true); err != nil {
		t.Errorf("allow-missing must not fail: %v", err)
	}
	if b, _ := os.ReadFile(ghOut); !strings.Contains(string(b), "available=false") {
		t.Errorf("GITHUB_OUTPUT = %q, want available=false", b)
	}
	if err := RunFetchFromState(d, "primary", outPath, false); err == nil {
		t.Error("empty kubeconfig_raw without allow-missing must fail")
	}

	// Failed init aborts before any output read — and is retried before giving up.
	prevSleep := tfInitSleep
	var initSleeps int
	tfInitSleep = func(time.Duration) { initSleeps++ }
	t.Cleanup(func() { tfInitSleep = prevSleep })
	var initCalls int
	TfInitStream = func(...string) error { initCalls++; return errors.New("backend error") }
	if err := RunFetchFromState(d, "primary", outPath, false); err == nil ||
		!strings.Contains(err.Error(), "terraform init failed") {
		t.Errorf("init failure: err = %v", err)
	}
	if initCalls != tfInitAttempts || initSleeps != tfInitAttempts-1 {
		t.Errorf("init tried %d times with %d sleeps, want %d/%d", initCalls, initSleeps, tfInitAttempts, tfInitAttempts-1)
	}
}
