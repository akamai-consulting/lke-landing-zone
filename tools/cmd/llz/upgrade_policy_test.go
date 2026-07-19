package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSnapshotUpgradeOwnedRestoresOwnedButNotCopierAnswers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".template-manifest"), "owned owned.txt\nowned .copier-answers.yml\nmanaged managed.txt\n")
	writeFile(t, filepath.Join(dir, "owned.txt"), "operator\n")
	writeFile(t, filepath.Join(dir, ".copier-answers.yml"), "llz_version: v0.1.0\n")
	writeFile(t, filepath.Join(dir, "managed.txt"), "old template\n")
	gitInitRepo(t, dir, ".template-manifest", "owned.txt", ".copier-answers.yml", "managed.txt")
	chdir(t, dir)

	m, err := loadTemplateManifest(".")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := snapshotUpgradeOwned(m)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.cleanup()

	writeFile(t, filepath.Join(dir, "owned.txt"), "copier clobbered\n")
	writeFile(t, filepath.Join(dir, ".copier-answers.yml"), "llz_version: v0.2.0\n")
	if err := snap.restore(); err != nil {
		t.Fatal(err)
	}

	if got := mustRead(t, filepath.Join(dir, "owned.txt")); got != "operator\n" {
		t.Errorf("owned restored = %q", got)
	}
	if got := mustRead(t, filepath.Join(dir, ".copier-answers.yml")); got != "llz_version: v0.2.0\n" {
		t.Errorf("copier answers should not be restored, got %q", got)
	}
}

func TestOverwriteManagedFromScaffoldOnlyCopiesManagedFiles(t *testing.T) {
	clean := t.TempDir()
	writeFile(t, filepath.Join(clean, ".template-manifest"), "managed .template-manifest\nmanaged managed.txt\nmerge merge.txt\nowned owned.txt\n")
	writeFile(t, filepath.Join(clean, "managed.txt"), "new template\n")
	writeFile(t, filepath.Join(clean, "merge.txt"), "new merge\n")
	writeFile(t, filepath.Join(clean, "owned.txt"), "new owned\n")

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "managed.txt"), "old managed\n")
	writeFile(t, filepath.Join(dir, "merge.txt"), "local merge\n")
	writeFile(t, filepath.Join(dir, "owned.txt"), "operator owned\n")
	chdir(t, dir)

	count, err := overwriteManagedFromScaffold(clean)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 { // managed.txt + .template-manifest
		t.Fatalf("managed overwrite count = %d, want 2", count)
	}
	if got := mustRead(t, filepath.Join(dir, "managed.txt")); got != "new template\n" {
		t.Errorf("managed file = %q", got)
	}
	if got := mustRead(t, filepath.Join(dir, "merge.txt")); got != "local merge\n" {
		t.Errorf("merge file should stay local, got %q", got)
	}
	if got := mustRead(t, filepath.Join(dir, "owned.txt")); got != "operator owned\n" {
		t.Errorf("owned file should stay local, got %q", got)
	}
}

// The upgrade's conflict gate is upgradeConflictFiles (runUpgrade), which scans
// what copier just changed — here, files left untracked in the worktree. A bare
// `=======` under a heading is markdown underline, not a conflict separator, so
// ok.md must not trip it.
func TestUpgradeConflictFilesFindsMergeMarkersNotMarkdownRules(t *testing.T) {
	dir := t.TempDir()
	gitInitRepo(t, dir)
	writeFile(t, filepath.Join(dir, "ok.md"), "Heading\n======= not a conflict separator because it has text\n")
	writeFile(t, filepath.Join(dir, "bad.yaml"), "apiVersion: v1\n<<<<<<< before updating\nkind: ConfigMap\n=======\nkind: Secret\n>>>>>>> after updating\n")
	chdir(t, dir)

	got := upgradeConflictFiles()
	want := []string{"bad.yaml"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upgradeConflictFiles = %v, want %v", got, want)
	}
}

func TestCopierRenderArgvUsesAnswersAndSkipsTasks(t *testing.T) {
	got := copierRenderArgv(&answers{SrcPath: "gh:my-org/lke-landing-zone", UpstreamOrg: "my-org", InstanceRepo: "my-org/inst"}, "v1.2.3", "/tmp/render")
	joined := strings.Join(got, " ")
	for _, want := range []string{"copier copy", "--skip-tasks", "--force", "--vcs-ref v1.2.3", "upstream_org=my-org", "instance_repo=my-org/inst", "llz_version=v1.2.3", "gh:my-org/lke-landing-zone", "/tmp/render"} {
		if !strings.Contains(joined, want) {
			t.Errorf("copierRenderArgv missing %q in %q", want, joined)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
