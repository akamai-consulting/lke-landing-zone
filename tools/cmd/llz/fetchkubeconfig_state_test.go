package main

import (
	"os"
	"path/filepath"
	"testing"
)

// instanceRootFrom walks up to the landingzone.yaml-bearing instance root, so
// fetch-kubeconfig-state can render the (gitignored) cluster root before init
// regardless of the cluster subdir it runs from.
func TestInstanceRootFrom(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "landingzone.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "terraform-iac-bootstrap", "cluster")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := instanceRootFrom(sub)
	if err != nil {
		t.Fatalf("should find the root from a nested dir: %v", err)
	}
	// macOS /var → /private/var symlink: compare resolved paths.
	if g, _ := filepath.EvalSymlinks(got); g != mustEval(t, root) {
		t.Errorf("instanceRootFrom = %s, want %s", got, root)
	}
	// No landingzone.yaml anywhere up-tree → error (renderRootsFn treats it as a
	// pre-generate-roots no-op).
	if _, err := instanceRootFrom(t.TempDir()); err == nil {
		t.Error("want error when no landingzone.yaml exists up-tree")
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// kubeconfigRawProblem gates what fetch-kubeconfig-state writes to disk: a
// corrupt value poisons every downstream kubectl with "yaml: control characters
// are not allowed" and masks the real failure the diag step exists to surface.
func TestKubeconfigRawProblem(t *testing.T) {
	valid := "apiVersion: v1\nkind: Config\nclusters:\n- name: lke\n  cluster:\n    server: https://x:6443\n"
	if got := kubeconfigRawProblem([]byte(valid)); got != "" {
		t.Errorf("valid kubeconfig flagged: %q", got)
	}
	cases := map[string]string{
		"empty":            "",
		"whitespace":       "   \n\t ",
		"control chars":    "apiVersion: v1\nclusters:\x00\x07 garbage",
		"not a kubeconfig": "just some\nrandom: text\n", // no apiVersion
	}
	for name, in := range cases {
		if kubeconfigRawProblem([]byte(in)) == "" {
			t.Errorf("%s: should be flagged as a problem, was accepted", name)
		}
	}
	// A valid kubeconfig with a tab/CR (legal YAML whitespace) is NOT rejected.
	if got := kubeconfigRawProblem([]byte("apiVersion: v1\r\nclusters: []\r\n")); got != "" {
		t.Errorf("CRLF kubeconfig wrongly flagged: %q", got)
	}
}
