package sustain

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// testDeps is the Deps a unit test hands the sustain verbs.
//
// Confirm defaults to true for the same reason teardown's does: the ACTING path is
// what a test should exercise unless it says otherwise, and a fixture quietly
// defaulting to dry-run would let every assertion pass against a no-op.
func testDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		ReadAnswers: func(string) (*Answers, error) { return nil, nil },
		Exec:        func(string, ...string) ([]byte, error) { return nil, nil },
		Run:         func([]string, string) error { return nil },
		Confirm:     func() bool { return true },
	}
}

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

func chdirTemp(t *testing.T) { chdir(t, t.TempDir()) }

// captureStdout — the drift verb's report IS its product, so cases assert on it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	b, _ := io.ReadAll(r)
	return string(b)
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

func gitTracked(t *testing.T, dir string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			got = append(got, l)
		}
	}
	sort.Strings(got)
	return got
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// realGitDeps is testDeps with the shell-outs wired to the REAL commands. The
// template-removals and churn-guard cases build an actual git repo in a temp dir
// and assert on what git reports afterwards, so stubbing the exec seam would make
// them assert against their own fixture.
func realGitDeps(t *testing.T) Deps {
	t.Helper()
	d := testDeps(t)
	d.Exec = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	}
	d.Run = func(argv []string, _ string) error {
		return exec.Command(argv[0], argv[1:]...).Run()
	}
	return d
}
