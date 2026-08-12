package selfupgrade

import (
	"path/filepath"
	"testing"
)

// The overwrite-managed test, which belongs here: its subject is
// overwriteManagedFromScaffold. It travelled to main with a sibling test whose
// subject really is commands.go, and came straight back.

// The upgrade-conflict test, returned to main: its subject is
// upgradeConflictFiles in commands.go. Filename-as-subject, fourteenth
// occurrence.
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

	count, _, err := overwriteManagedFromScaffold(clean)
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
