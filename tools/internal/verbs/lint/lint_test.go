package lint

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/manifestguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/gitcmd"
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
			if _, err := gitcmd.Output(dir, args...); err != nil {
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
	if _, err := gitcmd.Output(dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	if err := stepConflictMarkers(cliopts.Global); err != nil {
		t.Fatalf("clean tree should pass: %v", err)
	}

	// Now introduce a committed conflict marker in a tracked text file.
	write("kustomization.yaml", "resources:\n<<<<<<< before updating\n  - x\n=======\nresources: []\n>>>>>>> after updating\n")
	if _, err := gitcmd.Output(dir, "add", "kustomization.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := stepConflictMarkers(cliopts.Global); err == nil {
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
		if got := manifestguard.IsDeclaredAPIVersion(tc.line, api); got != tc.want {
			t.Errorf("manifestguard.IsDeclaredAPIVersion(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// droppedAPITree materializes files in a throwaway dir and cds in. Deliberately no
// git: the scan is a filesystem walk, because the Kubernetes lint job runs inside a
// container where git against the mounted checkout fails.
func droppedAPITree(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	chdir(t, dir)
}

func TestStepDroppedAPIVersionsClean(t *testing.T) {
	droppedAPITree(t, map[string]string{
		// Already migrated to v1.
		"kubernetes-charts/managed-apps/chart/templates/es.yaml": "apiVersion: external-secrets.io/v1\nkind: ExternalSecret\n",
		// A changelog PROSE mention of the dropped version is not a declaration.
		"kubernetes-charts/managed-apps/chart/Chart.yaml": "# 0.1.6: bumped external-secrets.io/v1beta1 -> v1\nname: managed-apps\n",
		// v1alpha1 is still served in 2.4.1 (PushSecret is v1alpha1-only) — not a hit.
		"platform-apl/components/harbor/push.yaml": "apiVersion: external-secrets.io/v1alpha1\nkind: PushSecret\n",
		// A tree nobody ships from stays out of scope even on the dropped version.
		"docs/designs/example.yaml": "apiVersion: external-secrets.io/v1beta1\nkind: ExternalSecret\n",
		// A VENDORED subchart `helm dep build` unpacked during the same lint run is
		// upstream content, not ours to gate — skipped even on the dropped version.
		"kubernetes-charts/llz-openbao-platform/charts/openbao/templates/es.yaml": "apiVersion: external-secrets.io/v1beta1\nkind: ExternalSecret\n",
	})
	if err := stepDroppedAPIVersions(cliopts.Global); err != nil {
		t.Fatalf("migrated manifests + prose mention + served v1alpha1 + out-of-scope docs/ + vendored subchart should pass: %v", err)
	}
}

func TestStepDroppedAPIVersionsFlagsEveryScannedTree(t *testing.T) {
	// Each scanned tree must trip independently: the operator-owned escape hatch,
	// the scaffold copy the template ships, the first-party charts, and the SHARED
	// platform-apl/ tree — the one no CRD-aware gate covers (k8s-lint/k8s-validate
	// and the kind dry-run all read $RENDER_DIR, built from kubernetes-charts/*/
	// only), which is how llz-cidr-firewall shipped on v1beta1.
	for _, f := range []string{
		"kubernetes-custom/namespaces/team-x/es.yaml",
		"instance-template/kubernetes-custom/namespaces/team-x/es.yaml",
		"kubernetes-charts/llz-thing/templates/es.yaml",
		"platform-apl/components/cidrFirewall/llz-cidr-firewall/externalsecret.yaml",
	} {
		t.Run(f, func(t *testing.T) {
			droppedAPITree(t, map[string]string{f: "apiVersion: external-secrets.io/v1beta1\nkind: ExternalSecret\n"})
			if err := stepDroppedAPIVersions(cliopts.Global); err == nil {
				t.Errorf("expected failure: %s declares external-secrets.io/v1beta1", f)
			}
		})
	}
}

// TestCIDroppedAPIVersionsNoGit is the regression for the CI break: the Kubernetes
// lint job runs inside the ci-kubernetes container, where git against the mounted
// checkout fails. A git-backed scan reported "not a git repo" and failed the gate
// on a perfectly clean tree, so the scan must not consult git at all.
func TestCIDroppedAPIVersionsNoGit(t *testing.T) {
	dir := t.TempDir() // no `git init` — deliberately not a repo
	p := filepath.Join(dir, "platform-apl/components/x/externalsecret.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("apiVersion: external-secrets.io/v1\nkind: ExternalSecret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := manifestguard.RunCIDroppedAPIVersions(dir); err != nil {
		t.Fatalf("clean tree outside a git repo must pass, got: %v", err)
	}
	// The empty-corpus guard still has to bite, or a moved tree passes silently.
	if err := manifestguard.RunCIDroppedAPIVersions(t.TempDir()); err == nil {
		t.Error("expected failure: no manifests examined at all (trees moved)")
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
	if err := stepRenderFresh(cliopts.Global); err != nil {
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
	err := stepRenderFresh(cliopts.Global)
	if err == nil {
		t.Fatal("expected stale/absent committed render output to fail lint")
	}
	if !strings.Contains(err.Error(), "llz render") {
		t.Errorf("the failure should tell the operator how to fix it, got: %v", err)
	}
}

// TestGoFmtGate covers the gofmt pre-commit gate. The bug it exists to prevent is
// structural, not cosmetic: `gofmt -l` reports drift on STDOUT and exits 0
// regardless, so a step that trusted the exit code would pass forever.
func TestGoFmtGate(t *testing.T) {
	t.Run("argv lists, it does not rewrite", func(t *testing.T) {
		argv := goFmtListArgv("gofmt", []string{"tools/cmd", "tools/internal"})
		want := []string{"gofmt", "-l", "tools/cmd", "tools/internal"}
		if !reflect.DeepEqual(argv, want) {
			t.Fatalf("argv = %v, want %v", argv, want)
		}
		for _, a := range argv {
			// -w would silently REWRITE the developer's staged tree from inside a
			// pre-commit hook, committing files they never saw.
			if a == "-w" {
				t.Fatal("argv contains -w: the gate must report drift, never rewrite source")
			}
		}
	})

	t.Run("output is the verdict, not the exit code", func(t *testing.T) {
		if got := goFmtUnformatted(""); len(got) != 0 {
			t.Fatalf("clean output parsed as %v, want none", got)
		}
		if got := goFmtUnformatted("\n  \n"); len(got) != 0 {
			t.Fatalf("blank-only output parsed as %v, want none", got)
		}
		got := goFmtUnformatted("cmd/llz/a.go\ncmd/llz/b.go\n")
		want := []string{"cmd/llz/a.go", "cmd/llz/b.go"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("skips where there is no Go tree", func(t *testing.T) {
		// An adopter instance repo vendors no Go source. Running there must be a
		// no-op pass, not a failure — same philosophy as a missing linter.
		dir := t.TempDir()
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(cwd) })
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		if got := goModuleDirs(); len(got) != 0 {
			t.Fatalf("goModuleDirs in an empty repo = %v, want none", got)
		}
		if err := stepGoFmt(cliopts.Opts{}); err != nil {
			t.Fatalf("stepGoFmt with no Go tree = %v, want nil (skip)", err)
		}
	})

	t.Run("wired into the lint gate", func(t *testing.T) {
		// A step nobody calls protects nothing. Compare code pointers, since func
		// values are not otherwise comparable in Go.
		want := reflect.ValueOf(stepGoFmt).Pointer()
		for _, s := range lintSteps() {
			if reflect.ValueOf(s).Pointer() == want {
				return
			}
		}
		t.Fatal("stepGoFmt is not in lintSteps() — the gate would never run it")
	})
}
