package configreadiness

// A COPY of package main's writeGoodReadiness fixture — it lays down a tfvars tree
// that readiness reports clean, which is what the CIDR tests need as a baseline.

import (
	"os"
	"path/filepath"
	"testing"
)

// writeGoodReadiness lays down a minimal, fully-consistent scaffold for env so a
// single mutation per test isolates the branch under test.
func writeGoodReadiness(t *testing.T, dir, env string) {
	t.Helper()
	writeTFVars(t, dir, "cluster", env, "region = \"us-ord\"\n")
	writeTFVars(t, dir, "cluster-bootstrap", env, "deployment = \""+env+"\"\napl_values_env = \""+env+"\"\n")
	writeTFVars(t, dir, "object-storage", env, "region_suffix = \""+env+"\"\nobj_cluster = \"us-ord-10\"\n")
	// databases is opt-in, but render always writes at least the region_suffix stub,
	// so a consistent scaffold carries the databases tfvars too.
	writeTFVars(t, dir, "databases", env, "region_suffix = \""+env+"\"\n")
	writeFileMkdir(t, filepath.Join(dir, "apl-values", env, "manifest", "kustomization.yaml"),
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n")
}

// writeFileMkdir writes content at path, creating parent dirs (mustWrite does not).
func writeFileMkdir(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, content)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
