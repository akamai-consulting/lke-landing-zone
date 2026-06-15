package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemplateVersion drops a .template-version into the current dir.
func writeTemplateVersion(t *testing.T, sha string) {
	t.Helper()
	b, err := json.Marshal(templateVersion{
		Schema: 1, TemplateRepo: "akamai/lke-landing-zone", TemplateRef: "main", TemplateSHA: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".template-version", b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// chdirTemp moves into a fresh temp dir for the duration of the test.
func chdirTemp(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

// driftStub dispatches the git shell-outs runDrift makes: ls-remote returns the
// remote head, cat-file reports reachable, rev-list returns the behind-count.
func driftStub(latest string) func(string, ...string) ([]byte, error) {
	return func(_ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "ls-remote"):
			return []byte(latest + "\trefs/heads/main\n"), nil
		case strings.Contains(joined, "rev-list"):
			return []byte("5\n"), nil
		default: // cat-file -e (commitReachable)
			return nil, nil
		}
	}
}

func TestRunDriftUpToDate(t *testing.T) {
	chdirTemp(t)
	writeTemplateVersion(t, "abcd1234")
	withExecOutput(t, driftStub("abcd1234")) // remote head == stamped sha

	var err error
	captureStdout(t, func() { err = runDrift("main", "", false) })
	if err != nil {
		t.Errorf("runDrift(up to date) = %v, want nil", err)
	}
}

func TestRunDriftDrifted(t *testing.T) {
	chdirTemp(t)
	writeTemplateVersion(t, "oldsha00")
	withExecOutput(t, driftStub("newsha99")) // remote head moved ahead

	// Non-strict: drift is reported but not an error.
	var err error
	captureStdout(t, func() { err = runDrift("main", "", false) })
	if err != nil {
		t.Errorf("runDrift(drifted, non-strict) = %v, want nil", err)
	}
	// Strict: drift is a hard failure.
	captureStdout(t, func() { err = runDrift("main", "", true) })
	if err == nil {
		t.Error("runDrift(drifted, strict) = nil, want error")
	}
}

func TestRunDriftMissingFile(t *testing.T) {
	chdirTemp(t)
	// No .template-version present.
	if err := runDrift("main", "", false); err == nil {
		t.Error("runDrift(no .template-version) = nil, want error")
	}
}

func TestRunDriftMalformed(t *testing.T) {
	chdirTemp(t)
	if err := os.WriteFile(filepath.Join(".", ".template-version"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runDrift("main", "", false); err == nil {
		t.Error("runDrift(malformed) = nil, want error")
	}
}
