package cli

import (
	"strings"
	"testing"
)

// Tests that travelled with build_preflight.go but exercise cmdBuild, which is
// commands.go's and stays in main. Filename-as-subject, thirteenth occurrence.

func TestCmdBuildSkipPreflightBypassesTheCheck(t *testing.T) {
	// The escape hatch has to actually work: a spec that deliberately lives
	// elsewhere (another branch, another checkout) must still be dispatchable.
	dir := t.TempDir()
	writeMiniInstance(t, dir) // no deployments at all — the preflight would refuse
	chdir(t, dir)
	stubGitHub(t, nil)

	if err := cmdBuild([]string{"lab"}, globalOpts{}, false, false); err == nil {
		t.Fatal("without --skip-preflight an unknown deployment must be refused")
	}
	if err := cmdBuild([]string{"lab"}, globalOpts{}, true, false); err != nil {
		t.Errorf("--skip-preflight must bypass the check, got %v", err)
	}
}

func TestBuildWatchRequiresYes(t *testing.T) {
	// Without --yes nothing is dispatched, so --watch would follow a run that was
	// never created. Caught before the dispatch, where the message can still say
	// something useful. Lives here rather than with the watcher's own tests
	// (internal/shared/dispatchwatch) because the guard is cmdBuild's, not the
	// watcher's — the watcher cannot refuse a dispatch that already happened.
	err := cmdBuild([]string{"lab"}, globalOpts{Yes: false}, true, true)
	if err == nil || !strings.Contains(err.Error(), "--watch requires --yes") {
		t.Errorf("--watch without --yes must be refused up front, got %v", err)
	}
}
