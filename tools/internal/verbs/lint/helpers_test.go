package lint

// helpers_test.go — the two helpers the moved tests use, local and minimal.
//
// withExecOutput swaps kubectlprobe.Exec, which is the ONE seam: deps.go's
// execOutput and gitcmd.Output are closures over it, so replacing it covers every
// shell-out this package makes.

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = prev })
}

// captureStderr collects what fn wrote to stderr — the lint steps' failure text
// is their deliverable, so it is what the mutation tests assert on.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	defer func() { os.Stderr = orig }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
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
