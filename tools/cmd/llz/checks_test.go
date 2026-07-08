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
		{"fmt-check-paths", fmtCheckArgvPaths("tofu", []string{"a/main.tf", "a/legacy.tfvars"}),
			[]string{"tofu", "fmt", "-check", "a/main.tf", "a/legacy.tfvars"}},
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

func TestConflictMarkerLines(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []int
	}{
		{"clean", "resources:\n  - a\n  - b\n", nil},
		{"git-conflict", "a\n<<<<<<< HEAD\nx\n=======\ny\n>>>>>>> other\nb\n", []int{2, 6}},
		{"copier-conflict", "resources:\n<<<<<<< before updating\n  - managed-apps\n=======\nresources: []\n>>>>>>> after updating\n", []int{2, 6}},
		{"bare-markers", "<<<<<<<\n=======\n>>>>>>>\n", []int{1, 3}},
		// Legitimate content that must NOT trip the scan.
		{"setext-underline", "Heading\n=======\ntext\n", nil},
		{"ascii-run-of-eight", "<<<<<<<< not a marker\n>>>>>>>> nope\n", nil},
		{"equals-only", "key: val\n=======\nother: val\n", nil},
	}
	for _, c := range cases {
		if got := conflictMarkerLines(c.content); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s\n got: %v\nwant: %v", c.name, got, c.want)
		}
	}
}

func TestStepConflictMarkers(t *testing.T) {
	if _, err := execLookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitInit := func() {
		for _, args := range [][]string{
			{"init", "-q"},
			{"config", "user.email", "t@t"},
			{"config", "user.name", "t"},
		} {
			if _, err := gitOutput(dir, args...); err != nil {
				t.Fatalf("git %v: %v", args, err)
			}
		}
	}
	gitInit()
	// A clean tracked file and a binary file with marker-looking bytes (must be skipped).
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("clean.yaml", "resources:\n  - a\n")
	write("bin.dat", "<<<<<<< HEAD\x00binary\n") // NUL → treated as binary, skipped
	if _, err := gitOutput(dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	if err := stepConflictMarkers(gopts); err != nil {
		t.Fatalf("clean tree should pass: %v", err)
	}

	// Now introduce a committed conflict marker in a tracked text file.
	write("kustomization.yaml", "resources:\n<<<<<<< before updating\n  - x\n=======\nresources: []\n>>>>>>> after updating\n")
	if _, err := gitOutput(dir, "add", "kustomization.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := stepConflictMarkers(gopts); err == nil {
		t.Fatal("expected failure on a tracked file with conflict markers")
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
