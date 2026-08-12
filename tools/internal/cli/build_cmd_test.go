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

	if err := cmdBuild([]string{"lab"}, globalOpts{}, false, false, false); err == nil {
		t.Fatal("without --skip-preflight an unknown deployment must be refused")
	}
	if err := cmdBuild([]string{"lab"}, globalOpts{}, true, false, false); err != nil {
		t.Errorf("--skip-preflight must bypass the check, got %v", err)
	}
}

func TestBuildWatchAcceptsAnAuthenticatedGH(t *testing.T) {
	// The ordinary LOCAL setup: no env token, but `gh auth login` has been run —
	// which is what the quickstart tells an operator to do. The first cut of the
	// credential check looked at the two env vars only, so it refused that
	// operator with a message about a CI secret, for a dispatch that would have
	// worked perfectly.
	orig := ghCanAuth
	t.Cleanup(func() { ghCanAuth = orig })
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	ghCanAuth = func() bool { return true }
	dir := t.TempDir()
	writeMiniInstance(t, dir)
	chdir(t, dir)
	stubGitHub(t, nil)
	// Reaches the dispatch rather than being refused up front; --dry-run keeps it
	// from actually running anything.
	if err := cmdBuild([]string{"lab"}, globalOpts{Yes: true, DryRun: true}, true, true, false); err != nil &&
		strings.Contains(err.Error(), "no GitHub credential") {
		t.Errorf("an authenticated gh must satisfy --watch, got %v", err)
	}

	ghCanAuth = func() bool { return false }
	err := cmdBuild([]string{"lab"}, globalOpts{Yes: true, DryRun: true}, true, true, false)
	if err == nil || !strings.Contains(err.Error(), "no GitHub credential") {
		t.Errorf("with nothing able to authenticate, --watch must be refused, got %v", err)
	}
}

func TestBuildWatchRequiresYes(t *testing.T) {
	// Without --yes nothing is dispatched, so --watch would follow a run that was
	// never created. Caught before the dispatch, where the message can still say
	// something useful. Lives here rather than with the watcher's own tests
	// (internal/shared/dispatchwatch) because the guard is cmdBuild's, not the
	// watcher's — the watcher cannot refuse a dispatch that already happened.
	err := cmdBuild([]string{"lab"}, globalOpts{Yes: false}, true, true, false)
	if err == nil || !strings.Contains(err.Error(), "--watch requires --yes") {
		t.Errorf("--watch without --yes must be refused up front, got %v", err)
	}
}
