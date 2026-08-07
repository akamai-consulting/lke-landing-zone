package main

import "testing"

// Tests that travelled with build_preflight.go but exercise cmdBuild, which is
// commands.go's and stays in main. Filename-as-subject, thirteenth occurrence.

func TestCmdBuildSkipPreflightBypassesTheCheck(t *testing.T) {
	// The escape hatch has to actually work: a spec that deliberately lives
	// elsewhere (another branch, another checkout) must still be dispatchable.
	dir := t.TempDir()
	writeMiniInstance(t, dir) // no deployments at all — the preflight would refuse
	chdir(t, dir)
	stubGitHub(t, nil)

	if err := cmdBuild([]string{"lab"}, globalOpts{}, false); err == nil {
		t.Fatal("without --skip-preflight an unknown deployment must be refused")
	}
	if err := cmdBuild([]string{"lab"}, globalOpts{}, true); err != nil {
		t.Errorf("--skip-preflight must bypass the check, got %v", err)
	}
}
