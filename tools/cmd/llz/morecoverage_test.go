package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// When the underlying tool isn't on PATH, every lint/validate step is a no-op
// pass — stubbing execLookPath absent drives that branch through both
// orchestrators and the standalone fmt-fix step.
func TestLintStepsSkipWhenToolsAbsent(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", errors.New("absent") })
	g := globalOpts{}
	if err := runLint(g); err != nil {
		t.Errorf("runLint (tools absent) = %v, want nil", err)
	}
	if err := runValidate(g); err != nil {
		t.Errorf("runValidate (tools absent) = %v, want nil", err)
	}
	if err := stepFmtFix(g); err != nil {
		t.Errorf("stepFmtFix (tools absent) = %v, want nil", err)
	}
}

func containsSub(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestMaskGHA(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	if out := captureStdout(t, func() { maskGHA("topsecret") }); !strings.Contains(out, "::add-mask::topsecret") {
		t.Errorf("maskGHA in GHA = %q, want an add-mask line", out)
	}
	// Empty value emits nothing even inside GHA.
	if out := captureStdout(t, func() { maskGHA("") }); out != "" {
		t.Errorf("maskGHA(\"\") = %q, want empty", out)
	}
	// Outside GHA, nothing is masked.
	t.Setenv("GITHUB_ACTIONS", "")
	if out := captureStdout(t, func() { maskGHA("topsecret") }); out != "" {
		t.Errorf("maskGHA outside GHA = %q, want empty", out)
	}
}

func TestOverlayScanFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "values.yaml"), "a: 1")
	mustWrite(t, filepath.Join(dir, "README.md"), "# docs") // excluded by extension
	sub := filepath.Join(dir, "manifest")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(sub, "patch.json"), "{}")

	files := overlayScanFiles(dir)
	for _, f := range files {
		if strings.EqualFold(filepath.Ext(f), ".md") {
			t.Errorf("overlayScanFiles included a markdown file: %s", f)
		}
	}
	if !containsSub(files, "values.yaml") || !containsSub(files, "patch.json") {
		t.Errorf("overlayScanFiles = %v, want values.yaml and patch.json", files)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHasMainBranchRule(t *testing.T) {
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "gh" || len(args) == 0 || args[0] != "api" {
			t.Errorf("hasMainBranchRule shelled out to %q %v, want gh api ...", name, args)
		}
		return []byte(`{"branch_policies":[{"name":"main"},{"name":"release/*"}]}`), nil
	})
	if !hasMainBranchRule("o/r", "infra-dev", "main") {
		t.Error("hasMainBranchRule(main present) = false, want true")
	}
	if hasMainBranchRule("o/r", "infra-dev", "develop") {
		t.Error("hasMainBranchRule(absent) = true, want false")
	}

	// A gh failure is reported as "no rule" (false), never a panic.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("gh down") })
	if hasMainBranchRule("o/r", "infra-dev", "main") {
		t.Error("hasMainBranchRule(gh error) = true, want false")
	}
}

func TestOpenbaoClient(t *testing.T) {
	for _, k := range []string{
		"OPENBAO_ADDR_ACTIVE", "OPENBAO_TOKEN_ACTIVE", "OPENBAO_TOKEN", "OPENBAO_NAMESPACE",
	} {
		t.Setenv(k, "")
	}
	if _, err := openbaoClient("bogus"); err == nil {
		t.Error("openbaoClient(bogus role) = nil, want error")
	}
	if _, err := openbaoClient(roleActive); err == nil {
		t.Error("openbaoClient(no addr) = nil, want error")
	}
	t.Setenv("OPENBAO_ADDR_ACTIVE", "https://bao.example")
	if _, err := openbaoClient(roleActive); err == nil {
		t.Error("openbaoClient(no token) = nil, want error")
	}
	t.Setenv("OPENBAO_TOKEN_ACTIVE", "tok")
	c, err := openbaoClient(roleActive)
	if err != nil || c == nil {
		t.Errorf("openbaoClient(addr+token) = (%v, %v), want a client", c, err)
	}
}

func TestRunOpenbaoPathValidation(t *testing.T) {
	// Both commands reject a path outside the secret/ KV v2 mount up front.
	if err := runOpenbaoGet("active", "not-secret/x", "k"); err == nil {
		t.Error("runOpenbaoGet(bad path) = nil, want error")
	}
	if err := runOpenbaoSet(globalOpts{}, "not-secret/x", []string{"k=v"}); err == nil {
		t.Error("runOpenbaoSet(bad path) = nil, want error")
	}
	// A malformed key=value pair is caught before any OpenBao call.
	if err := runOpenbaoSet(globalOpts{}, "secret/app", []string{"noequals"}); err == nil {
		t.Error("runOpenbaoSet(no '=') = nil, want error")
	}
	// No pairs at all is a usage error.
	if err := runOpenbaoSet(globalOpts{}, "secret/app", nil); err == nil {
		t.Error("runOpenbaoSet(no pairs) = nil, want error")
	}
}

func TestReportArgoHealthDryRun(t *testing.T) {
	// Dry-run returns nil without ever shelling out to kubectl.
	called := false
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err := reportArgoHealth(globalOpts{dryRun: true}, false, 0); err != nil {
		t.Errorf("reportArgoHealth(dry-run) = %v, want nil", err)
	}
	if called {
		t.Error("reportArgoHealth(dry-run) shelled out, want no exec")
	}
}
