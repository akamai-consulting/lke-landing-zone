package lint

// tracked_fmt_targets_test.go — moved with trackedFmtTargets, which the fmt
// steps use to list what is worth formatting. The rest of dynamic_tfvars_test.go
// stayed in package main: it tests untrackRenderedTfvars, which is internal/cli's.

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// trackedFmtTargets must list tracked *.tf / *.tfvars under a root while skipping
// the untracked (gitignored) rendered tfvars and the *.tfvars.example default.
func TestTrackedFmtTargets(t *testing.T) {
	dir := t.TempDir()
	root := "terraform-iac-bootstrap/cluster"
	writeFile(t, filepath.Join(dir, root, "main.tf"), "# module\n")
	writeFile(t, filepath.Join(dir, root, "terraform.tfvars.example"), "region = \"us-x\"\n")
	writeFile(t, filepath.Join(dir, root, "legacy.tfvars"), "region = \"us-y\"\n") // tracked (legacy source of truth)
	writeFile(t, filepath.Join(dir, root, "prod.tfvars"), "region = \"us-z\"\n")   // untracked (rendered) → skipped
	gitInitRepo(t, dir, root+"/main.tf", root+"/terraform.tfvars.example", root+"/legacy.tfvars")
	chdir(t, dir)

	got, ok := trackedFmtTargets(root)
	if !ok {
		t.Fatal("trackedFmtTargets returned ok=false inside a git repo")
	}
	sort.Strings(got)
	want := []string{root + "/legacy.tfvars", root + "/main.tf"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("trackedFmtTargets\n got: %v\nwant: %v (prod.tfvars untracked, .example excluded)", got, want)
	}
}

func TestTrackedFmtTargets_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if _, ok := trackedFmtTargets("terraform-iac-bootstrap/cluster"); ok {
		t.Error("expected ok=false outside a git repo (caller falls back to the dir scan)")
	}
}
