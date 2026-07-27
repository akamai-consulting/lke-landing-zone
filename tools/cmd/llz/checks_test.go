package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestIsDeclaredAPIVersion(t *testing.T) {
	const api = "external-secrets.io/v1beta1"
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"apiVersion: external-secrets.io/v1beta1", true},
		{"  apiVersion: external-secrets.io/v1beta1", true},
		{`apiVersion: "external-secrets.io/v1beta1"`, true},
		{"apiVersion: external-secrets.io/v1beta1  # legacy", true},
		{"apiVersion: external-secrets.io/v1", false},                  // already migrated
		{"# bump apiVersion external-secrets.io/v1beta1 -> v1", false}, // prose/comment, not a key
		{"  key: external-secrets.io/v1beta1", false},                  // some other key
	} {
		if got := isDeclaredAPIVersion(tc.line, api); got != tc.want {
			t.Errorf("isDeclaredAPIVersion(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestStepDroppedAPIVersions(t *testing.T) {
	if _, err := execLookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		if _, err := gitOutput(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	mk := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Owned tree already on v1 → clean. A changelog PROSE mention of v1beta1 → clean.
	// A v1beta1 manifest in a TEMPLATE-managed tree (platform-apl/) is out of scope.
	mk("kubernetes-charts/managed-apps/chart/templates/es.yaml", "apiVersion: external-secrets.io/v1\nkind: ExternalSecret\n")
	mk("kubernetes-charts/managed-apps/chart/Chart.yaml", "# 0.1.6: bumped external-secrets.io/v1beta1 -> v1\nname: managed-apps\n")
	mk("platform-apl/components/x/externalsecret.yaml", "apiVersion: external-secrets.io/v1beta1\nkind: ExternalSecret\n")
	if _, err := gitOutput(dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	if err := stepDroppedAPIVersions(gopts); err != nil {
		t.Fatalf("clean owned tree + prose mention + out-of-scope platform-apl should pass: %v", err)
	}

	// A v1beta1 manifest in an OWNED tree (kubernetes-custom/) must fail.
	mk("kubernetes-custom/namespaces/team-x/es.yaml", "apiVersion: external-secrets.io/v1beta1\nkind: ExternalSecret\n")
	if _, err := gitOutput(dir, "add", "kubernetes-custom/namespaces/team-x/es.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := stepDroppedAPIVersions(gopts); err == nil {
		t.Fatal("expected failure: owned kubernetes-custom manifest on external-secrets.io/v1beta1")
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

// stepRenderFresh must be silent outside an instance — the template repo has no
// LandingZone spec of its own, and lint runs there on every commit.
func TestStepRenderFreshSkipsWithoutASpec(t *testing.T) {
	chdir(t, t.TempDir())
	if err := stepRenderFresh(gopts); err != nil {
		t.Fatalf("no spec present must skip, got: %v", err)
	}
}

// The gap this closes: `llz render --check` existed but nothing an instance runs
// called it, so committed apl-values could lag the pin indefinitely (one instance
// was three releases behind, changing what ArgoCD actually deployed). A spec whose
// committed output is missing must now fail the lint gate.
func TestStepRenderFreshCatchesStaleCommittedOutput(t *testing.T) {
	dir := t.TempDir()
	spec := `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata:
  name: t
spec:
  instance:
    upstreamOrg: akamai-consulting
    repo: o/t
    forge: github
  environments:
    prod:
      cluster:
        clusterLabel: t-prod
        region: us-ord
        k8sVersion: v1.33.6+lke7
        nodePool: { type: g8-dedicated-8-4, count: 3 }
        bootstrap: { name: t-prod, managedAppPlatform: true }
`
	if err := os.WriteFile(filepath.Join(dir, "landingzone.yaml"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	// Nothing rendered yet, so every committed apl-values target is missing — the
	// same shape as output left behind by an older pin.
	err := stepRenderFresh(gopts)
	if err == nil {
		t.Fatal("expected stale/absent committed render output to fail lint")
	}
	if !strings.Contains(err.Error(), "llz render") {
		t.Errorf("the failure should tell the operator how to fix it, got: %v", err)
	}
}
