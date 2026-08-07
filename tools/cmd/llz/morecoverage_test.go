package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/converge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/openbao"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghsecret"
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
	if out := captureStdout(t, func() { ghsecret.Mask("topsecret") }); !strings.Contains(out, "::add-mask::topsecret") {
		t.Errorf("ghsecret.Mask in GHA = %q, want an add-mask line", out)
	}
	// Empty value emits nothing even inside GHA.
	if out := captureStdout(t, func() { ghsecret.Mask("") }); out != "" {
		t.Errorf("ghsecret.Mask(\"\") = %q, want empty", out)
	}
	// Outside GHA, nothing is masked.
	t.Setenv("GITHUB_ACTIONS", "")
	if out := captureStdout(t, func() { ghsecret.Mask("topsecret") }); out != "" {
		t.Errorf("ghsecret.Mask outside GHA = %q, want empty", out)
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

	files := configreadiness.OverlayScanFiles(dir)
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

func TestOpenbaoClient(t *testing.T) {
	for _, k := range []string{
		"OPENBAO_ADDR_ACTIVE", "OPENBAO_TOKEN_ACTIVE", "OPENBAO_TOKEN", "OPENBAO_NAMESPACE",
	} {
		t.Setenv(k, "")
	}
	if _, err := openbao.NewClientFor("bogus"); err == nil {
		t.Error("openbao.NewClientFor(bogus role) = nil, want error")
	}
	if _, err := openbao.NewClientFor(envtopology.RoleActive); err == nil {
		t.Error("openbao.NewClientFor(no addr) = nil, want error")
	}
	t.Setenv("OPENBAO_ADDR_ACTIVE", "https://bao.example")
	if _, err := openbao.NewClientFor(envtopology.RoleActive); err == nil {
		t.Error("openbao.NewClientFor(no token) = nil, want error")
	}
	t.Setenv("OPENBAO_TOKEN_ACTIVE", "tok")
	c, err := openbao.NewClientFor(envtopology.RoleActive)
	if err != nil || c == nil {
		t.Errorf("openbao.NewClientFor(addr+token) = (%v, %v), want a client", c, err)
	}
}

func TestRunOpenbaoPathValidation(t *testing.T) {
	// Both commands reject a path outside the secret/ KV v2 mount up front.
	if err := openbao.RunGet("active", "not-secret/x", "k"); err == nil {
		t.Error("openbao.RunGet(bad path) = nil, want error")
	}
	if err := openbao.RunSet(false, false, "not-secret/x", []string{"k=v"}); err == nil {
		t.Error("openbao.RunSet(bad path) = nil, want error")
	}
	// A malformed key=value pair is caught before any OpenBao call.
	if err := openbao.RunSet(false, false, "secret/app", []string{"noequals"}); err == nil {
		t.Error("openbao.RunSet(no '=') = nil, want error")
	}
	// No pairs at all is a usage error.
	if err := openbao.RunSet(false, false, "secret/app", nil); err == nil {
		t.Error("openbao.RunSet(no pairs) = nil, want error")
	}
}

func TestReportArgoHealthDryRun(t *testing.T) {
	// Dry-run returns nil without ever shelling out to kubectl.
	called := false
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err := converge.ReportArgoHealth(true, false, 0); err != nil {
		t.Errorf("converge.ReportArgoHealth(dry-run) = %v, want nil", err)
	}
	if called {
		t.Error("converge.ReportArgoHealth(dry-run) shelled out, want no exec")
	}
}
