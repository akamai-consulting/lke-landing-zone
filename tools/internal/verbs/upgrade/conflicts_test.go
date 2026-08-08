package upgrade

// The upgrade-conflict test: its subject is conflictFiles in commands.go.
// The other test in this file went the other way — it was about
// overwriteManagedFromScaffold, which is internal/selfupgrade's. Two tests, one
// old file, two different subjects.

import (
	"path/filepath"
	"reflect"
	"testing"
)

// The upgrade-conflict test, returned to main: its subject is
// conflictFiles in commands.go. Filename-as-subject, fourteenth
// occurrence.

// The upgrade's conflict gate is conflictFiles (runUpgrade), which scans
// what copier just changed — here, files left untracked in the worktree. A bare
// `=======` under a heading is markdown underline, not a conflict separator, so
// ok.md must not trip it.
func TestUpgradeConflictFilesFindsMergeMarkersNotMarkdownRules(t *testing.T) {
	dir := t.TempDir()
	gitInitRepo(t, dir)
	writeFile(t, filepath.Join(dir, "ok.md"), "Heading\n======= not a conflict separator because it has text\n")
	writeFile(t, filepath.Join(dir, "bad.yaml"), "apiVersion: v1\n<<<<<<< before updating\nkind: ConfigMap\n=======\nkind: Secret\n>>>>>>> after updating\n")
	chdir(t, dir)

	got := conflictFiles()
	want := []string{"bad.yaml"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("conflictFiles = %v, want %v", got, want)
	}
}
