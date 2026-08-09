package upgrade

// helpers_test.go — gitInitRepo, copied rather than shared.
//
// This is the FOURTH copy (cmd/llz, internal/sustain, internal/selfupgrade, here).
// A shared testutil package would collapse them, and that is deliberately not done:
// each copy is ~18 lines with no production caller, and hoisting it would put a
// non-test package in the tree whose only job is to be imported by tests. The
// duplication is cheaper than the dependency, and the three prior copies drifted in
// no way that mattered.

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
)

// TestMain installs the SustainDeps seam. In production main installs it before
// any command runs; in tests nothing does, and currentTemplateRef dereferences it
// on the first call — so the package's own tests have to stand in for main.
//
// The stand-in is deliberately NOT a copy of main's: LockableScaffoldFiles and
// the --yes Confirm are the two fields that made this a seam rather than a plain
// function, and neither is reachable from anything tested here.
func TestMain(m *testing.M) {
	SustainDeps = func() sustain.Deps {
		return sustain.Deps{
			ReadAnswers: func(dir string) (*sustain.Answers, error) {
				a, err := answers.Read(dir)
				if err != nil || a == nil {
					return nil, err
				}
				return &sustain.Answers{Commit: a.Commit, SrcPath: a.SrcPath, Version: a.Version}, nil
			},
			Exec:    func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).Output() },
			Run:     proc.Run,
			Confirm: func() bool { return false },
		}
	}
	os.Exit(m.Run())
}

// chdir cds into dir for the duration of the test, restoring the cwd after.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
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

// gitInitRepo makes dir a git repo and commits every path in `add` (relative to
// dir), so the files live in HEAD — a conflict scan reads committed content.
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

// captureStdoutStderr swaps both streams for pipes so a test can read what a
// print-only function emitted. reportCIImageSkew's entire deliverable is its
// stderr, so there is nothing else to assert on.
func captureStdoutStderr(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wo, we
	fn()
	wo.Close()
	we.Close()
	o, _ := io.ReadAll(ro)
	e, _ := io.ReadAll(re)
	return string(o), string(e)
}

const repinSHA = "b9fe2721b55e2cb196d418f8d0bc6069957e3bd3"

// pinnedAt puts the test in an instance pinned at ref whose images all resolve.
//
// MINIMAL BY CONSTRUCTION: templatecommit's copy of this name reaches for a
// writeInstanceDir/stubTemplateCommit/stubImagePublished trio that exists to
// exercise the RESOLVER. Nothing here resolves anything — reportCIImageSkew only
// reads what StaleCIImageVars returns — so the two seams are stubbed inline and
// the instance dir is one chdir and one file.
func pinnedAt(t *testing.T, ref string) {
	t.Helper()
	dir := t.TempDir()
	chdir(t, dir)
	writeFile(t, filepath.Join(dir, ".copier-answers.yml"),
		"_src_path: gh:acme/tmpl\nllz_version: "+ref+"\n")

	prevResolve, prevIP := templatecommit.Resolve, templatecommit.ImagePublished
	templatecommit.Resolve = func(string, string) (string, bool) { return repinSHA, true }
	templatecommit.ImagePublished = func(string) (bool, bool) { return true, true }
	t.Cleanup(func() { templatecommit.Resolve, templatecommit.ImagePublished = prevResolve, prevIP })
}
