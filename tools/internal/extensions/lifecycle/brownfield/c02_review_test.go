package brownfield

// c02_review_test.go — the gates for the C02 dry-run findings of the 2026-08-13
// review. `--yes` is AUTHORISATION for something destructive; `--dry-run` is a
// request to describe rather than act. Two steps of the adoption path had the
// read-only one gated on the destructive one.

import (
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// TestScanWritesTheReportWithoutYes.
//
// `llz import scan`'s own help says "Purely read-only; no --yes needed", and every
// example in docs/runbooks/import-apl-site.md omits --yes. Gated on Confirm, the
// FIRST STEP of the documented adoption path printed "(dry-run) … would write
// import-report.yaml", exited 0, and wrote nothing — and step two,
// `llz import init`, then failed on a report that was never created.
func TestScanWritesTheReportWithoutYes(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "import-report.yaml")

	d := Deps{
		// Not set: this is what `llz import scan` with no --yes and no
		// --dry-run looks like.
		DryRun:     func() bool { return false },
		KubectlOut: func(...string) (string, error) { return "", nil },
	}
	if err := RunScan(d, ScanOpts{Output: out, SkipCluster: true}); err != nil {
		t.Fatalf("a read-only scan must not need --yes: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("the scan wrote no report without --yes. `llz import init` is documented to read this "+
			"file, so the whole adoption path stops here: %v", err)
	}
}

// TestScanHonoursDryRun pins the other side — --dry-run must still describe
// rather than write, or the flag means nothing here.
func TestScanHonoursDryRun(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "import-report.yaml")
	d := Deps{
		DryRun:     func() bool { return true },
		KubectlOut: func(...string) (string, error) { return "", nil },
	}
	if err := RunScan(d, ScanOpts{Output: out, SkipCluster: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("--dry-run must not write the report")
	}
}

// TestComponentTogglesApplyWithoutYes.
//
// RunInit's other three steps — scaffold, author the spec, write
// MIGRATION-TODO.md — all run for real without --yes. Gating this one on Confirm
// meant the documented `llz import init` produced a complete instance with EVERY
// component the scan had discovered left OFF, announcing it with a "(dry-run)"
// line in the middle of a run that plainly was not one.
func TestComponentTogglesApplyWithoutYes(t *testing.T) {
	// The assignments come from the real producer, not a hand-written literal.
	// A literal `spec.components.harbor.enabled=true` compiles and passes while
	// stating a contract that does not exist — enabledComponentAssignments emits
	// no `spec.` prefix — so the test would keep passing across a change to either
	// side of a split neither side owns alone.
	assigns := enabledComponentAssignments(importReport{
		Platform: importPlatform{Components: map[string]bool{"harbor": true}},
	})
	if len(assigns) != 1 {
		t.Fatalf("fixture drift: enabledComponentAssignments produced %v", assigns)
	}
	var edited bool
	var setPaths []string
	d := Deps{
		DryRun:      func() bool { return false },
		EnvSpecFile: func(string) (string, error) { return "environments/prod.yaml", nil },
		EditSpec: func(_ string, mutate func(*yamlv3.Node) error, _ func([]byte) error) error {
			edited = true
			var doc yamlv3.Node
			if err := yamlv3.Unmarshal([]byte("spec: {}\n"), &doc); err != nil {
				return err
			}
			return mutate(&doc)
		},
		SetSpecPath: func(_ *yamlv3.Node, dotted, value string) error {
			setPaths = append(setPaths, dotted+"="+value)
			return nil
		},
		Render: func(string) error { return nil },
	}
	if err := applyComponentToggles(d, "prod", assigns); err != nil {
		t.Fatalf("applying toggles must not need --yes: %v", err)
	}
	if !edited || len(setPaths) != 1 || setPaths[0] != assigns[0] {
		t.Errorf("the toggles the scan found were not applied (edited=%v set=%v) — the operator gets an "+
			"instance with every discovered component off", edited, setPaths)
	}
}

// TestInitHonoursDryRunForEVERYStep pins the other side, at the level the flag
// actually has to hold. --dry-run started life inside applyComponentToggles
// alone, which meant `llz import init --dry-run` scaffolded a real instance and
// wrote a real MIGRATION-TODO.md while printing "(dry-run)" about the toggles in
// the middle of it — a flag half the steps observe makes the "(dry-run)" line
// evidence for a claim that is false.
func TestInitHonoursDryRunForEVERYStep(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "import-report.yaml")
	if err := os.WriteFile(report, []byte("platform:\n  components:\n    harbor: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := Deps{
		DryRun:          func() bool { return true },
		ValidateEnvName: func(string) error { return nil },
		New: func(string, string, string) error {
			t.Error("--dry-run must not scaffold an instance")
			return nil
		},
		EnvAdd: func(string, EnvSpec) error {
			t.Error("--dry-run must not author a spec")
			return nil
		},
		EditSpec: func(string, func(*yamlv3.Node) error, func([]byte) error) error {
			t.Error("--dry-run must not edit the spec")
			return nil
		},
	}
	if err := RunInit(d, InitOpts{Report: report, Dir: filepath.Join(dir, "inst"), Env: "prod"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "inst", migrationTodoFile)); err == nil {
		t.Error("--dry-run wrote MIGRATION-TODO.md — the checklist is a real file the operator then edits")
	}
}

// TestDryRunSeamIsAClosureNotASnapshot. brownfieldDeps is built while the command
// TREE is being constructed, which is before cobra parses persistent flags — so a
// captured `cliopts.Global.DryRun` would be frozen false forever. That exact
// defect shipped in converge's installConvergeDeps and made `--dry-run` mutate a
// live cluster; this asserts the shape that prevents it, from this side.
func TestDryRunSeamIsAClosureNotASnapshot(t *testing.T) {
	flag := false
	d := Deps{DryRun: func() bool { return flag }}
	if d.DryRun() {
		t.Fatal("unset")
	}
	flag = true // what cobra does, after the tree exists
	if !d.DryRun() {
		t.Error("Deps.DryRun must read the flag when CALLED, not when the Deps were built")
	}
}
