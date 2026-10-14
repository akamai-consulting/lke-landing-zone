package selfupgrade

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// THE SPLIT CONTRACT THIS PINS. copierRenderArgv stopped passing --skip-tasks so
// the clean render would be the artifact a fresh `llz new` produces. That made the
// render's correctness depend on something the argv cannot express: copier's
// `_tasks` invoke `llz` BY NAME and degrade to a warning when it is not on PATH,
// and a degraded render delivers the UNPRUNED docs tree that the overwrite pass
// then copies into the instance.
//
// `llz ci upgrade-test` cannot catch it: the gate arms PATH itself (putOnPATH), so
// it observes a render that production does not perform. Two halves of one rule,
// each fine alone — the same shape as the defect this PR was opened to fix.

// stubRender swaps both seams and returns the recorded call order.
func stubRender(t *testing.T, runErr error) *[]string {
	t.Helper()
	var order []string
	prevSelf, prevRun := selfOnPATH, runProc
	selfOnPATH = func(name string) (func(), error) {
		order = append(order, "selfOnPATH("+name+")")
		return func() { order = append(order, "restorePATH") }, nil
	}
	runProc = func(argv []string, _ string) error {
		order = append(order, "run("+argv[0]+")")
		return runErr
	}
	t.Cleanup(func() { selfOnPATH, runProc = prevSelf, prevRun })
	return &order
}

func TestRenderUpgradeScaffoldArmsLLZBeforeCopierRuns(t *testing.T) {
	t.Chdir(t.TempDir()) // no .copier-answers.yml: copierRenderArgv falls back to defaults
	order := stubRender(t, nil)

	_, cleanup, err := renderUpgradeScaffold("v0.0.42")
	if err != nil {
		t.Fatalf("renderUpgradeScaffold: %v", err)
	}
	defer cleanup()

	got := strings.Join(*order, " → ")
	want := "selfOnPATH(llz) → run(copier) → restorePATH"
	if got != want {
		t.Errorf("copier's `llz`-by-name tasks are not armed around the render.\n  want %s\n  got  %s\n"+
			"  A render whose tasks take the no-llz fallback delivers an UNPRUNED docs/ tree,\n"+
			"  which the managed-overwrite pass then copies into the instance.", want, got)
	}
}

// Fail closed. If the binary cannot be published, the render must not proceed —
// a silently degraded render is worse than a refused upgrade, because its output
// is copied over the instance and looks like a successful delivery.
func TestRenderUpgradeScaffoldRefusesToRenderWhenLLZCannotBePublished(t *testing.T) {
	t.Chdir(t.TempDir())
	var ran bool
	prevSelf, prevRun := selfOnPATH, runProc
	selfOnPATH = func(string) (func(), error) { return func() {}, errors.New("no executable") }
	runProc = func([]string, string) error { ran = true; return nil }
	t.Cleanup(func() { selfOnPATH, runProc = prevSelf, prevRun })

	if _, _, err := renderUpgradeScaffold("v0.0.42"); err == nil {
		t.Fatal("rendered anyway — the tasks would have degraded silently")
	}
	if ran {
		t.Error("copier ran despite `llz` being unpublishable")
	}
}

// The temp dir must not leak when the arming fails, same as on a copier failure.
func TestRenderUpgradeScaffoldCleansUpOnBothFailurePaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		selfErr error
		runErr  error
	}{
		{"arming fails", errors.New("no executable"), nil},
		{"copier fails", nil, errors.New("copier exploded")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			before := tempDirEntries(t)

			prevSelf, prevRun := selfOnPATH, runProc
			selfOnPATH = func(string) (func(), error) { return func() {}, tc.selfErr }
			runProc = func([]string, string) error { return tc.runErr }
			t.Cleanup(func() { selfOnPATH, runProc = prevSelf, prevRun })

			if _, _, err := renderUpgradeScaffold("v0.0.42"); err == nil {
				t.Fatal("expected an error")
			}
			if after := tempDirEntries(t); after > before {
				t.Errorf("leaked %d llz-upgrade-render-* dir(s)", after-before)
			}
		})
	}
}

func tempDirEntries(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read %s: %v", os.TempDir(), err)
	}
	var n int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "llz-upgrade-render-") {
			n++
		}
	}
	return n
}
