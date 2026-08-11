package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stageExecutable writes an executable file under name and returns its path. It
// stands in for the llz binary: what matters to every test here is the FILE
// IDENTITY the child resolves, not what the file does.
func stageExecutable(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("stage %s: %v", name, err)
	}
	return path
}

// withExecutable points the osExecutable seam at path for one test.
func withExecutable(t *testing.T, path string) {
	t.Helper()
	prev := osExecutable
	osExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { osExecutable = prev })
}

// THE REGRESSION THIS FILE EXISTS FOR. The binary is not named like the command
// copier looks up — `llz-422` while bisecting, a `go run` temp binary — so the
// `PATH=$(dirname $self)` shape publishes nothing findable and copier's tasks
// take their no-llz fallback. That fallback is what copies the UNPRUNED docs tree
// into the instance once the clean render stopped passing --skip-tasks.
func TestSelfOnPATHPublishesABinaryNotNamedLikeTheCommand(t *testing.T) {
	self := stageExecutable(t, "llz-422")
	withExecutable(t, self)
	t.Setenv("PATH", t.TempDir()) // an empty PATH: nothing named llz anywhere

	restore, err := SelfOnPATH("llz")
	if err != nil {
		t.Fatalf("SelfOnPATH: %v", err)
	}
	defer restore()

	found, err := exec.LookPath("llz")
	if err != nil {
		t.Fatalf("`llz` does not resolve after SelfOnPATH — copier's tasks would take the "+
			"no-llz fallback and leave the docs tree unpruned: %v", err)
	}
	assertSameFile(t, found, self)
}

// A child process — which is what copier and its `_tasks` are — must resolve it,
// not just this one. LookPath reads our own env; exec.Command hands it to a fork.
func TestSelfOnPATHIsInheritedByChildren(t *testing.T) {
	self := stageExecutable(t, "llz-under-another-name")
	withExecutable(t, self)
	// /bin:/usr/bin, not an empty dir: the child IS `sh`, so it has to stay
	// resolvable. Neither directory carries an llz.
	t.Setenv("PATH", "/bin:/usr/bin")

	restore, err := SelfOnPATH("llz")
	if err != nil {
		t.Fatalf("SelfOnPATH: %v", err)
	}
	defer restore()

	// Exactly the probe copier.yml's tasks use.
	out, err := exec.Command("sh", "-c", "command -v llz").Output()
	if err != nil {
		t.Fatalf("`command -v llz` failed in a child: %v", err)
	}
	assertSameFile(t, strings.TrimSpace(string(out)), self)
}

// The installed-llz case. Publishing a second copy would also work, but a no-op
// keeps the normal upgrade from allocating a temp dir and rewriting PATH.
func TestSelfOnPATHNoOpsWhenPATHAlreadyResolvesToSelf(t *testing.T) {
	self := stageExecutable(t, "llz")
	withExecutable(t, self)
	t.Setenv("PATH", filepath.Dir(self))

	before := os.Getenv("PATH")
	restore, err := SelfOnPATH("llz")
	if err != nil {
		t.Fatalf("SelfOnPATH: %v", err)
	}
	defer restore()

	if got := os.Getenv("PATH"); got != before {
		t.Errorf("PATH was rewritten when it already resolved `llz` to this binary:\n  before %q\n  after  %q", before, got)
	}
}

// A DIFFERENT llz on PATH is not good enough, and this is the adopter case: an
// older release installed system-wide while the operator runs a newer binary by
// path. The tasks must run the binary performing the upgrade.
func TestSelfOnPATHOverridesADifferentLLZAlreadyOnPATH(t *testing.T) {
	other := stageExecutable(t, "llz")
	self := stageExecutable(t, "llz")
	withExecutable(t, self)
	t.Setenv("PATH", filepath.Dir(other))

	restore, err := SelfOnPATH("llz")
	if err != nil {
		t.Fatalf("SelfOnPATH: %v", err)
	}
	defer restore()

	found, err := exec.LookPath("llz")
	if err != nil {
		t.Fatalf("LookPath: %v", err)
	}
	assertSameFile(t, found, self)
}

func TestSelfOnPATHCleanupRestoresPATHAndRemovesTheStagedDir(t *testing.T) {
	self := stageExecutable(t, "llz-422")
	withExecutable(t, self)
	t.Setenv("PATH", t.TempDir())
	before := os.Getenv("PATH")

	restore, err := SelfOnPATH("llz")
	if err != nil {
		t.Fatalf("SelfOnPATH: %v", err)
	}
	dir := filepath.SplitList(os.Getenv("PATH"))[0]
	if dir == "" || dir == before {
		t.Fatal("nothing was prepended to PATH")
	}

	restore()

	if got := os.Getenv("PATH"); got != before {
		t.Errorf("PATH not restored:\n  want %q\n  got  %q", before, got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("staged dir %s survived cleanup (err=%v)", dir, err)
	}
}

// Fail closed. A caller that cannot publish itself must hear about it rather than
// proceed into a render whose tasks will silently degrade.
func TestSelfOnPATHReportsAnUnresolvableExecutable(t *testing.T) {
	withExecutable(t, filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("PATH", t.TempDir())

	restore, err := SelfOnPATH("llz")
	if err == nil {
		restore()
		t.Fatal("no error for an executable that does not exist — the caller would render with degraded tasks")
	}
	if restore == nil {
		t.Fatal("cleanup must be non-nil even on the error path; callers defer it unconditionally")
	}
}

func assertSameFile(t *testing.T, got, want string) {
	t.Helper()
	gotInfo, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat %s: %v", got, err)
	}
	wantInfo, err := os.Stat(want)
	if err != nil {
		t.Fatalf("stat %s: %v", want, err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Errorf("`llz` resolved to %s, want %s", got, want)
	}
}
