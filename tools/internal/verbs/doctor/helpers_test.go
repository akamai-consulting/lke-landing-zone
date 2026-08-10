package doctor

// helpers_test.go — fixtures the moved tests need.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdirTemp lived in driftrun_test.go, which moved to internal/sustain with the
// drift verb. Other package-main tests still use it.
func chdirTemp(t *testing.T) {
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
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote — these helpers print a human report we don't want in test output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

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
