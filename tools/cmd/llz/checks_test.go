package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCheckArgvBuilders(t *testing.T) {
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{"fmt", fmtArgv("tofu", "terraform/cluster"),
			[]string{"tofu", "fmt", "terraform/cluster"}},
		{"fmt-check", fmtCheckArgv("tofu", "terraform/cluster"),
			[]string{"tofu", "fmt", "-check", "terraform/cluster"}},
		{"tflint", tfLintArgv("tflint", "terraform/cluster", "/abs/.tflintrc.hcl"),
			[]string{"tflint", "--chdir=terraform/cluster", "--config=/abs/.tflintrc.hcl"}},
		{"actionlint", actionsLintArgv("actionlint", []string{".github/workflows/ci.yml"}),
			[]string{"actionlint", ".github/workflows/ci.yml"}},
		{"gitleaks", gitleaksArgv("gitleaks"),
			[]string{"gitleaks", "detect", "--source", ".", "--no-banner"}},
		{"tf-init", tfInitArgv("terraform", "terraform/cluster"),
			[]string{"terraform", "-chdir=terraform/cluster", "init", "-backend=false", "-input=false"}},
		{"tf-validate", tfValidateArgv("terraform", "terraform/cluster"),
			[]string{"terraform", "-chdir=terraform/cluster", "validate"}},
		{"checkov", checkovArgv("checkov", "terraform/cluster"),
			[]string{"checkov", "-d", "terraform/cluster", "--framework", "terraform",
				"--config-file", ".checkov.yaml", "--compact", "--quiet"}},
	}
	for _, c := range cases {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s\n got: %v\nwant: %v", c.name, c.got, c.want)
		}
	}
}

func TestTFDirsKeepsOnlyExisting(t *testing.T) {
	dir := t.TempDir()
	// Create two of the four candidate roots (and a stray file that must be ignored).
	for _, d := range []string{"terraform-iac-bootstrap/cluster", "terraform-iac-bootstrap/object-storage"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "terraform-iac-bootstrap", "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)

	got := tfDirs()
	want := []string{"terraform-iac-bootstrap/cluster", "terraform-iac-bootstrap/object-storage"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tfDirs\n got: %v\nwant: %v", got, want)
	}
}

func TestToolEnvOverride(t *testing.T) {
	if got := tool("tofu", "LLZ_TOFU"); got != "tofu" {
		t.Errorf("default: got %q", got)
	}
	t.Setenv("LLZ_TOFU", "/opt/bin/opentofu")
	if got := tool("tofu", "LLZ_TOFU"); got != "/opt/bin/opentofu" {
		t.Errorf("override: got %q", got)
	}
}

// chdir cds into dir for the duration of the test, restoring the cwd after.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}
