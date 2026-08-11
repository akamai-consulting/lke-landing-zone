package render

// if_spec_test.go — `llz render --if-spec` is a NO-OP, not an error, on an
// instance that has no LandingZone spec.
//
// WHY THE FLAG EXISTS. The no-op contract used to live in the caller: every
// delivered CI step that rendered wrote
//
//	if [ -f landingzone.yaml ]; then llz render …; fi
//
// three lines of shell restating a rule the binary already knows, once per call
// site, in the category untestable-loc measures and refuses to grow. Moving it
// here makes it one implementation with one test — and one that asks the same
// question Run asks (layout detection) rather than testing for a filename
// relative to whatever working directory a workflow step happened to have.
//
// BOTH DIRECTIONS ARE PINNED. A flag that swallowed every failure would pass the
// first test below and quietly turn a broken spec into a green render, which is
// the vacuous-green shape this tree refuses: --if-spec must skip an ABSENT spec
// and nothing else.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderIn runs `llz render` with args in dir and returns its error.
func renderIn(t *testing.T, dir string, args ...string) error {
	t.Helper()
	t.Chdir(dir)
	c := RenderCmd()
	c.SetArgs(args)
	c.SetOut(os.Stderr)
	c.SilenceUsage, c.SilenceErrors = true, true
	return c.Execute()
}

func TestIfSpecIsANoOpWithoutASpec(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "terraform-iac-bootstrap", "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := renderIn(t, dir, "--tfvars-only", "--if-spec"); err != nil {
		t.Fatalf("--if-spec must no-op on a pre-spec instance, got: %v", err)
	}
}

// Without the flag the same tree must still FAIL. This is what stops --if-spec
// from being read as "the command is optional now": a step that means to render
// and cannot say so loudly.
func TestWithoutIfSpecAMissingSpecStillFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "terraform-iac-bootstrap", "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := renderIn(t, dir, "--tfvars-only")
	if err == nil {
		t.Fatal("a missing spec must fail a plain `llz render` — otherwise every caller silently renders nothing")
	}
	if !strings.Contains(err.Error(), "LandingZone spec") {
		t.Errorf("expected a spec-not-found diagnosis, got: %v", err)
	}
}

// --if-spec skips an ABSENT spec, never an INVALID one. Anything else would turn
// a spec the operator broke into a green render of stale artifacts.
func TestIfSpecStillFailsOnABrokenSpec(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "terraform-iac-bootstrap", "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "landingzone.yaml"), []byte("this: [is not: a spec\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := renderIn(t, dir, "--tfvars-only", "--if-spec"); err == nil {
		t.Error("--if-spec swallowed a broken spec — it must only skip a spec that is ABSENT")
	}
}

// SpecPresent must resolve the spec root the way Run does, so the flag cannot
// skip a render Run would have performed.
func TestSpecPresentAgreesWithRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "terraform-iac-bootstrap", "cluster"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if SpecPresent() {
		t.Fatal("SpecPresent is true with no spec on disk")
	}
	if err := os.WriteFile(filepath.Join(dir, "landingzone.yaml"), []byte("apiVersion: llz/v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !SpecPresent() {
		t.Error("SpecPresent is false with a spec on disk — --if-spec would skip every render")
	}
}
