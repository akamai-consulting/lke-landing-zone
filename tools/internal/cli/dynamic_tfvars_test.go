package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/render"
)

// gitInitRepo makes dir a git repo and commits every path in `add` (relative to
// dir), so the files live in HEAD — matching a real instance (where `git rm`
// removes committed files, not just staged ones).
func gitInitRepo(t *testing.T, dir string, add ...string) {
	t.Helper()
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "-q")
	if len(add) > 0 {
		runGit(append([]string{"add", "--"}, add...)...)
		runGit("commit", "-q", "-m", "fixture")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitTracked(t *testing.T, dir string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git ls-files: %v\n%s", err, out)
	}
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	sort.Strings(files)
	return files
}

// render.UntrackRenderedArtifacts drops tracked per-env tfvars from the index (the one-time
// migration) while leaving terraform.tfvars.example tracked; idempotent.
func TestUntrackRenderedArtifacts(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "") // force the local (non-CI) path
	dir := t.TempDir()
	tracked := []string{
		"terraform-iac-bootstrap/cluster/lab.tfvars",
		"terraform-iac-bootstrap/cluster-bootstrap/lab.tfvars",
		"terraform-iac-bootstrap/object-storage/lab.tfvars",
		"terraform-iac-bootstrap/vpc/web.tfvars",
		"terraform-iac-bootstrap/cluster/terraform.tfvars.example",
		"terraform-iac-bootstrap/cluster/main.tf",
	}
	for _, p := range tracked {
		writeFile(t, filepath.Join(dir, p), "x = 1\n")
	}
	gitInitRepo(t, dir, tracked...)
	chdir(t, dir)

	render.UntrackRenderedArtifacts("") // relPrefix "" = a real instance repo

	got := gitTracked(t, dir)
	want := []string{
		"terraform-iac-bootstrap/cluster/main.tf",
		"terraform-iac-bootstrap/cluster/terraform.tfvars.example",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("after untrack, tracked files\n got: %v\nwant: %v", got, want)
	}

	// Idempotent: a second call is a clean no-op.
	render.UntrackRenderedArtifacts("")
	if got2 := gitTracked(t, dir); strings.Join(got2, ",") != strings.Join(want, ",") {
		t.Errorf("second untrack changed the index: %v", got2)
	}
}

func TestUntrackRenderedArtifacts_NoOpInCIAndTemplate(t *testing.T) {
	dir := t.TempDir()
	p := "terraform-iac-bootstrap/cluster/lab.tfvars"
	writeFile(t, filepath.Join(dir, p), "x = 1\n")
	gitInitRepo(t, dir, p)
	chdir(t, dir)

	// CI: index must stay pristine (the migration is a local, committed action).
	t.Setenv("GITHUB_ACTIONS", "true")
	render.UntrackRenderedArtifacts("")
	if got := gitTracked(t, dir); len(got) != 1 || got[0] != p {
		t.Errorf("CI path should be a no-op; tracked: %v", got)
	}

	// In-template dev layout (relPrefix != "") is also a no-op.
	t.Setenv("GITHUB_ACTIONS", "")
	render.UntrackRenderedArtifacts("some/prefix")
	if got := gitTracked(t, dir); len(got) != 1 || got[0] != p {
		t.Errorf("template-layout path should be a no-op; tracked: %v", got)
	}
}
