package database

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/tofudriver"
)

// writeDBTfvars lays down terraform-iac-bootstrap/databases/<region>.tfvars in a
// temp cwd, mirroring what `llz render` produces.
func writeDBTfvars(t *testing.T, region, body string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	root := filepath.Join(dir, "terraform-iac-bootstrap", "databases")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if body != "" {
		if err := os.WriteFile(filepath.Join(root, region+".tfvars"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// ghaOutput points $GITHUB_OUTPUT (or another GHA file var) at a temp file and
// returns a reader for what the command appended.
func ghaOutput(t *testing.T, envVar string) func() string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gha")
	t.Setenv(envVar, path)
	return func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func TestDBDeclaredDetectsTheAssignment(t *testing.T) {
	// The rendered shapes that matter: DatabasesTFVars always writes
	// region_suffix, and adds `databases = {…}` ONLY when the spec declared one.
	for _, tc := range []struct {
		name, body string
		want       string
	}{
		{"declared", "region_suffix = \"prod\"\ndatabases = {\n  shared = {\n    region = \"us-ord\"\n  }\n}\n", "declared=true"},
		{"indented assignment", "region_suffix = \"prod\"\n  databases = {\n  }\n", "declared=true"},
		{"none declared", "region_suffix = \"prod\"\n", "declared=false"},
		// A comment mentioning the variable is not a declaration — the example
		// tfvars carries exactly this prose.
		{"comment only", "region_suffix = \"prod\"\n# databases = { … } to declare one\n", "declared=false"},
		// A different variable that merely starts with the same word.
		{"lookalike variable", "region_suffix = \"prod\"\ndatabases_enabled = true\n", "declared=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeDBTfvars(t, "prod", tc.body)
			read := ghaOutput(t, "GITHUB_OUTPUT")
			if err := RunDBDeclared("prod"); err != nil {
				t.Fatalf("db-declared: %v", err)
			}
			if got := strings.TrimSpace(read()); got != tc.want {
				t.Errorf("GITHUB_OUTPUT = %q, want %q", got, tc.want)
			}
		})
	}
}

// A deployment that never rendered the databases root (or predates it) is "none
// declared", not a failure — otherwise every pre-existing instance's bootstrap
// breaks on upgrade.
func TestDBDeclaredTreatsAMissingTfvarsAsNone(t *testing.T) {
	writeDBTfvars(t, "prod", "")
	read := ghaOutput(t, "GITHUB_OUTPUT")
	if err := RunDBDeclared("prod"); err != nil {
		t.Fatalf("a missing tfvars must not fail: %v", err)
	}
	if got := strings.TrimSpace(read()); got != "declared=false" {
		t.Errorf("GITHUB_OUTPUT = %q, want declared=false", got)
	}
}

func TestDBDeclaredRequiresRegion(t *testing.T) {
	if err := RunDBDeclared(""); err == nil {
		t.Fatal("expected --region to be required")
	}
}

func TestDBApplySummaryDistinguishesProvisionedFromNone(t *testing.T) {
	// Nothing provisioned must read as a deliberate no-op, not a silent failure.
	for _, empty := range []string{"", "null", "{}", "  "} {
		got := strings.Join(dbApplySummary("prod", empty), "\n")
		if !strings.Contains(got, "nothing provisioned") {
			t.Errorf("labels %q should render the no-op summary, got:\n%s", empty, got)
		}
	}

	got := strings.Join(dbApplySummary("prod", `{"shared":"platform-shared-prod"}`), "\n")
	for _, want := range []string{
		"Managed Postgres clusters provisioned (prod)",
		"platform-shared-prod",
		"bootstrap-openbao.yml",
		"secret/infra/db-admin/<name>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
}

func TestDBSummaryPhaseRouting(t *testing.T) {
	prev := tofudriver.OutputRunFn
	t.Cleanup(func() { tofudriver.OutputRunFn = prev })
	tofudriver.OutputRunFn = func() (string, error) {
		return `{"labels":{"value":{"shared":"platform-shared-prod"}}}`, nil
	}

	t.Run("apply reads the labels output", func(t *testing.T) {
		read := ghaOutput(t, "GITHUB_STEP_SUMMARY")
		if err := RunDBSummary("prod", "apply"); err != nil {
			t.Fatalf("db-summary apply: %v", err)
		}
		if !strings.Contains(read(), "platform-shared-prod") {
			t.Errorf("apply summary did not report the provisioned label:\n%s", read())
		}
	})

	t.Run("destroy-plan warns about data loss", func(t *testing.T) {
		read := ghaOutput(t, "GITHUB_STEP_SUMMARY")
		if err := RunDBSummary("prod", "destroy-plan"); err != nil {
			t.Fatalf("db-summary destroy-plan: %v", err)
		}
		out := read()
		for _, want := range []string{"[!CAUTION]", "irreversibly", "pg_dump", "NOT removed"} {
			if !strings.Contains(out, want) {
				t.Errorf("destroy warning missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("an unknown phase is rejected", func(t *testing.T) {
		if err := RunDBSummary("prod", "apply-maybe"); err == nil {
			t.Fatal("expected an unknown --phase to error")
		}
		if err := RunDBSummary("prod", ""); err == nil {
			t.Fatal("expected an empty --phase to error")
		}
	})

	t.Run("region is required", func(t *testing.T) {
		if err := RunDBSummary("", "apply"); err == nil {
			t.Fatal("expected --region to be required")
		}
	})
}
