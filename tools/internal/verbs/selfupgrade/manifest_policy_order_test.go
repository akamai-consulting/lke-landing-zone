package selfupgrade

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE WEDGE THIS PREVENTS. The clean render shells out to copier, which runs the
// template's `_tasks` — arbitrary shell, `cp -a <src>/docs/.` among them, which
// exits non-zero on a fork whose template carries no docs/. While that render
// lived INSIDE the apply, it ran after `copier update` had already rewritten the
// instance, so a task failure left the tree merged by copier but missing the
// `managed` overwrite and `.template-removals`, with nothing to roll back to.
//
// Splitting prepare/apply is only worth anything if the caller keeps the order,
// so both halves are pinned: that a failed render happens before anything is
// applied, and that Apply consumes the ALREADY-rendered scaffold rather than
// making a second one.

func TestPrepareManifestPolicyFailsBeforeAnythingIsApplied(t *testing.T) {
	instance := t.TempDir()
	t.Chdir(instance)
	// The instance as the operator left it. Nothing in a failed prepare may touch it.
	if err := os.WriteFile(filepath.Join(instance, "AGENTS.md"), []byte("operator content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prevSelf, prevRun := selfOnPATH, runProc
	selfOnPATH = func(string) (func(), error) { return func() {}, nil }
	runProc = func([]string, string) error { return errors.New("copier task exited 1") }
	t.Cleanup(func() { selfOnPATH, runProc = prevSelf, prevRun })

	p, err := PrepareManifestPolicy(false, "v0.0.42")
	if err == nil {
		t.Fatal("a failed render must be an error at PREPARE time — the instance has not been touched yet")
	}
	if p != nil {
		t.Error("no policy handle should be returned for a render that failed — the caller could Apply it")
	}
	if !strings.Contains(err.Error(), "render target scaffold") {
		t.Errorf("error does not say the render is what failed: %v", err)
	}

	// THE POINT OF THE SPLIT, asserted rather than assumed: the instance is exactly
	// as it was. This is what "half-upgraded with no rollback" looked like when the
	// render lived inside Apply, after copier update had already rewritten the tree.
	entries, err := os.ReadDir(instance)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "AGENTS.md" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("a failed prepare wrote into the instance: %v", names)
	}
	got, err := os.ReadFile(filepath.Join(instance, "AGENTS.md"))
	if err != nil || string(got) != "operator content\n" {
		t.Errorf("a failed prepare modified the instance's own file: %q (%v)", got, err)
	}
}

// Apply must not render. If it did, the fragile step would be back after the
// instance was mutated and the split would buy nothing.
func TestApplyDoesNotRenderAgain(t *testing.T) {
	t.Chdir(t.TempDir())
	var renders int
	prevSelf, prevRun := selfOnPATH, runProc
	selfOnPATH = func(string) (func(), error) { return func() {}, nil }
	// Stand in for copier: the overwrite pass reads .template-manifest out of the
	// rendered scaffold, so the stub has to leave one behind. dst is copier's last
	// argument.
	runProc = func(argv []string, _ string) error {
		renders++
		dst := argv[len(argv)-1]
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, ".template-manifest"), []byte("managed  .template-manifest\n"), 0o644)
	}
	t.Cleanup(func() { selfOnPATH, runProc = prevSelf, prevRun })

	p, err := PrepareManifestPolicy(false, "v0.0.42")
	if err != nil {
		t.Fatalf("PrepareManifestPolicy: %v", err)
	}
	defer p.Cleanup()
	if renders != 1 {
		t.Fatalf("prepare ran %d render(s), want 1", renders)
	}
	if err := p.Apply(UpgradeSnapshot{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if renders != 1 {
		t.Errorf("Apply rendered again (%d total) — the fragile step is back after the instance was mutated", renders)
	}
}

// --dry-run must not shell out at all, and must still hand back a usable handle
// so the caller's defer/Apply path is the same shape.
func TestDryRunPreparesWithoutRendering(t *testing.T) {
	t.Chdir(t.TempDir())
	var renders int
	prevRun := runProc
	runProc = func([]string, string) error { renders++; return nil }
	t.Cleanup(func() { runProc = prevRun })

	p, err := PrepareManifestPolicy(true, "v0.0.42")
	if err != nil {
		t.Fatalf("PrepareManifestPolicy(dry-run): %v", err)
	}
	defer p.Cleanup()
	if renders != 0 {
		t.Errorf("dry-run rendered %d time(s) — it must change nothing and shell out to nothing", renders)
	}
	if err := p.Apply(UpgradeSnapshot{}); err != nil {
		t.Errorf("dry-run Apply: %v", err)
	}
}

// Cleanup on a nil handle is the shape the caller uses: `var p *ManifestPolicy`
// stays nil when the manifest did not load, and the deferred Cleanup still runs.
func TestCleanupOnANilPolicyIsSafe(t *testing.T) {
	var p *ManifestPolicy
	p.Cleanup()
}
