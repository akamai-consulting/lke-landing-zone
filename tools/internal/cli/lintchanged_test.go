package cli

// lintchanged_test.go — the gate suite's own front door.
//
// `make lint` decides which checks to run from a file set, and computed that set
// as `git diff --name-only HEAD`, which DOES NOT LIST UNTRACKED FILES. A branch
// whose work so far is new files produced an empty set, took the "nothing
// changed" arm, and exited 0 having run no linters and no gates — over a tree
// that was entirely new code. Every other guard in this repo sits behind that
// arm, so it is the one worth testing directly.
//
// It runs the REAL `lint-changed` target against a throwaway git repository
// rather than re-deriving the git incantation here: restating the rule in the
// test is how both halves of a contract end up passing while the shipped one is
// blind. What is asserted is the set the Makefile actually prints.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoMakefile locates this repo's Makefile from the test's own source path, so
// the test does not depend on where `go test` was invoked from.
func repoMakefile(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot locate this test's source path")
	}
	// tools/internal/cli/<file> → repo root is three levels up from tools/.
	mk := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "Makefile")
	abs, err := filepath.Abs(mk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("Makefile not found at %s: %v", abs, err)
	}
	return abs
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// lintChanged runs the real target in dir and returns the paths it printed.
func lintChanged(t *testing.T, dir string) []string {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not on PATH")
	}
	cmd := exec.Command("make", "-f", repoMakefile(t), "--no-print-directory", "lint-changed")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("make lint-changed: %v\n%s", err, out)
	}
	var paths []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", ".")
	if err := os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "init")
	return dir
}

// THE DEFECT, DIRECTLY. A tree whose only work is new files must not look empty.
func TestLintSeesUntrackedFiles(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "brand_new.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := lintChanged(t, dir)
	if len(got) == 0 {
		t.Fatal("an untracked-only tree reported NO changed files — `make lint` would exit 0 having run " +
			"no linters and no gates over an entirely new file")
	}
	if !contains(got, "brand_new.go") {
		t.Errorf("changed set = %v, want it to include the untracked file", got)
	}
}

// AND STILL SEES THE TRACKED ONES, together, deduplicated. A fix that swapped
// one blind spot for another passes the test above.
func TestLintSeesTrackedAndUntrackedTogether(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("package x // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "brand_new.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := lintChanged(t, dir)
	for _, want := range []string{"tracked.go", "brand_new.go"} {
		if !contains(got, want) {
			t.Errorf("changed set = %v, want it to include %q", got, want)
		}
	}
	if n := count(got, "tracked.go"); n != 1 {
		t.Errorf("%q appears %d times, want 1 — the union is not deduplicated", "tracked.go", n)
	}
}

// A GITIGNORED FILE IS NOT WORK. Without --exclude-standard every build artifact
// joins the set and the changed-file routing stops meaning anything.
func TestLintIgnoresGitignoredFiles(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".gitignore")
	git(t, dir, "commit", "-qm", "ignore")
	if err := os.MkdirAll(filepath.Join(dir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build", "out.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range lintChanged(t, dir) {
		if strings.HasPrefix(p, "build/") {
			t.Errorf("changed set includes the gitignored %q", p)
		}
	}
}

// A GENUINELY CLEAN TREE IS STILL EMPTY, so the "nothing changed" arm keeps
// working and `make lint` on an untouched checkout stays fast.
func TestLintReportsNothingForACleanTree(t *testing.T) {
	if got := lintChanged(t, newRepo(t)); len(got) != 0 {
		t.Errorf("a clean tree reported %v, want nothing", got)
	}
}

// "COULD NOT TELL" IS NOT "NOTHING TO DO". A repo with no commits cannot answer
// `git diff HEAD`, and the fallback must lint EVERYTHING rather than collapsing
// into the same empty set as a clean tree — the two answers arriving at one
// string is what made the original bug invisible.
//
// BOTH SUB-CASES, BECAUSE THE FIRST CUT OF THIS TEST RAN ONLY THE STAGED ONE AND
// THAT IS THE ONE INPUT THAT HID THE BUG. The fallback was `git ls-files`, which
// lists TRACKED files — and `git add -A` is what makes a file tracked. So the
// test passed while a repository with no commits and nothing staged, which is
// the ordinary shape of the state that reaches this arm, still reported an empty
// set out of the target whose entire purpose is not to.
func TestLintFallsBackToEverythingWhenGitCannotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage bool
	}{
		{"staged but never committed", true},
		{"nothing staged at all", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			git(t, dir, "init", "-q", ".")
			if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.stage {
				git(t, dir, "add", "-A") // tracked, but there is still no HEAD
			}
			got := lintChanged(t, dir)
			if !contains(got, "a.go") {
				t.Errorf("changed set = %v, want the whole tree when git cannot diff against HEAD", got)
			}
		})
	}
}

// THE FALLBACK RESPECTS .gitignore TOO. It is the arm that lints everything, not
// the arm that lints build output — and the untracked listing it gained is the
// one that could drag it in.
func TestTheFallbackStillIgnoresGitignoredFiles(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", ".")
	for name, body := range map[string]string{
		".gitignore": "build/\n",
		"a.go":       "package x\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build", "out.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := lintChanged(t, dir)
	if !contains(got, "a.go") {
		t.Errorf("changed set = %v, want the real source file", got)
	}
	for _, p := range got {
		if strings.HasPrefix(p, "build/") {
			t.Errorf("the fallback listed the gitignored %q", p)
		}
	}
}

func contains(ss []string, want string) bool { return count(ss, want) > 0 }

func count(ss []string, want string) int {
	n := 0
	for _, s := range ss {
		if s == want {
			n++
		}
	}
	return n
}
