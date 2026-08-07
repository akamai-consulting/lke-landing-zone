package identityconfig

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Helpers the moved tests use, copied across the new package boundary.

// captureStderr mirrors captureStdout for the os.Stderr path (the remediation /
// warning printers write there).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
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

// writeResolveSpec writes a minimal split-layout spec (landingzone.yaml +
// environments/<env>.yaml) with just the domainSuffix resolve-harbor-url reads.
func writeResolveSpec(t *testing.T, dir, env, domainSuffix string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "landingzone.yaml"),
		[]byte("apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: itest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "environments"), 0o755); err != nil {
		t.Fatal(err)
	}
	cd := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata:\n  name: " + env +
		"\nspec:\n  cluster:\n    bootstrap:\n      domainSuffix: " + domainSuffix + "\n"
	if err := os.WriteFile(filepath.Join(dir, "environments", env+".yaml"), []byte(cd), 0o644); err != nil {
		t.Fatal(err)
	}
}
