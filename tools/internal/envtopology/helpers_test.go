package envtopology

// A COPY of package main's chdirTempDir fixture.

import (
	"os"
	"path/filepath"
	"testing"
)

// chdirTempDir moves the test into a fresh temp dir (the commands resolve tfvars
// relative to the workflow's checkout root).
func chdirTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return dir
}

// writeSpecInstance lays a minimal spec-driven instance into the current dir: a
// landingzone.yaml + one environments/<env>.yaml per (name, body) pair. Only the
// spec YAMLs are needed — loadSpec/readTopology read those, not the tfvars.
func writeSpecInstance(t *testing.T, envs map[string]string) {
	t.Helper()
	writeFileMkdir(t, "landingzone.yaml", `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: inst }
spec:
  instance: { upstreamOrg: o, repo: o/inst, forge: github, templateVersion: main }
  defaults:
    cluster:
      k8sVersion: v1.33.6+lke7
      nodePool: { type: g8-dedicated-8-4, count: 5 }
`)
	for name, body := range envs {
		writeFileMkdir(t, filepath.Join("environments", name+".yaml"), body)
	}
}

// withExecOutput swaps this package's Exec seam for one test.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := caps.Exec
	caps.Exec = fn
	t.Cleanup(func() { caps.Exec = orig })
}

func clusterDef(name, extra string) string {
	return `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: ` + name + ` }
spec:
  cluster:
    clusterLabel: inst-` + name + `
    region: us-ord
    bootstrap: { name: inst-` + name + ` }
    objectStorage: { cluster: us-ord-1 }
` + extra
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFileMkdir writes content at path, creating parent dirs (mustWrite does not).
func writeFileMkdir(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, content)
}
