package clusteraccess

// Termination coverage for fetch-kubeconfig-state's loops.
//
// Two of them can run away rather than fail: instanceRootFrom's walk up the
// tree, and tfInitWithRetry's bounded retry. instanceRootFrom is the dangerous
// one, because renderRootsFn hands its result to `exec.Command(os.Executable(),
// "render", ...)` — and under `go test` os.Executable() is THIS test binary. A
// walk that answers "found" (or "found: <empty path>") where it should answer
// "no instance root" therefore re-enters the whole suite recursively and the
// run HANGS instead of failing. renderReexecChild (wired into TestMain) turns
// that re-entry into an immediate, observable exit so these tests can assert
// exactly WHETHER the shell-out happened.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// renderReexecMarkerEnv names a file the re-entered binary touches, so the test
// that provoked the shell-out can prove it happened (and record the argv).
const renderReexecMarkerEnv = "LLZ_TEST_RENDER_REEXEC_MARKER"

// renderReexecChild reports whether this process was started by renderRootsFn's
// `<self> render <env> --tfvars-only` shell-out instead of by `go test`. TestMain
// exits immediately when it is: the alternative is running the entire suite once
// per shell-out, recursively.
func renderReexecChild() bool {
	if len(os.Args) < 2 || os.Args[1] != "render" {
		return false
	}
	if marker := os.Getenv(renderReexecMarkerEnv); marker != "" {
		cwd, _ := os.Getwd()
		_ = os.WriteFile(marker, []byte(strings.Join(os.Args[1:], " ")+"\n"+cwd+"\n"), 0o600)
	}
	return true
}

// readRenderMarker returns the argv + cwd the re-entered binary recorded, and
// whether the shell-out happened at all.
func readRenderMarker(t *testing.T, marker string) (argv, cwd string, ran bool) {
	t.Helper()
	b, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("reading the re-exec marker: %v", err)
	}
	parts := strings.SplitN(strings.TrimRight(string(b), "\n"), "\n", 2)
	if len(parts) != 2 {
		t.Fatalf("malformed re-exec marker %q", b)
	}
	return parts[0], parts[1], true
}

// armRenderMarker points the re-exec guard at a fresh (absent) marker file.
func armRenderMarker(t *testing.T) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "render-reexec")
	t.Setenv(renderReexecMarkerEnv, marker)
	return marker
}

// Inside an instance, renderRootsFn regenerates the gitignored cluster root:
// the instance commits ZERO Terraform, so `terraform init` would run against an
// empty directory without this. It must shell out to `llz render <env>
// --tfvars-only` FROM THE INSTANCE ROOT (where landingzone.yaml + the copier
// answers live), not from the cluster subdir the composite action runs in.
func TestRenderRootsFn_ShellsOutFromTheInstanceRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "landingzone.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "terraform-iac-bootstrap", "cluster")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := armRenderMarker(t)
	t.Chdir(sub)

	if err := renderRootsFn("primary"); err != nil {
		t.Fatalf("renderRootsFn: %v", err)
	}
	argv, cwd, ran := readRenderMarker(t, marker)
	if !ran {
		t.Fatal("renderRootsFn did not render the cluster root — terraform init would run against a directory with no *.tf")
	}
	if want := "render primary --tfvars-only"; argv != want {
		t.Errorf("shelled out with %q, want %q", argv, want)
	}
	if got, want := mustEval(t, cwd), mustEval(t, root); got != want {
		t.Errorf("rendered from %s, want the instance root %s", got, want)
	}
}

// Outside an instance (a pre-generate-roots instance whose roots ARE committed)
// there is nothing to render, and the shell-out must not happen at all: the
// render would run from an undefined directory with no spec to read.
func TestRenderRootsFn_NoInstanceRootIsANoOp(t *testing.T) {
	marker := armRenderMarker(t)
	t.Chdir(t.TempDir()) // no landingzone.yaml anywhere up-tree

	if err := renderRootsFn("primary"); err != nil {
		t.Fatalf("no instance root is a no-op, not an error: %v", err)
	}
	if argv, cwd, ran := readRenderMarker(t, marker); ran {
		t.Fatalf("renderRootsFn shelled out (%q from %q) with no landingzone.yaml up-tree", argv, cwd)
	}
}

// instanceRootFrom's contract, stated as the two answers renderRootsFn branches
// on. Both must be reachable: an "" path with a nil error, or a "found" verdict
// for a directory that holds no landingzone.yaml, sends the render off to the
// wrong place (and, under `go test`, back into this binary).
func TestInstanceRootFrom_AnswersAreUsable(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "landingzone.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := instanceRootFrom(root)
	if err != nil {
		t.Fatalf("instanceRootFrom on the root itself: %v", err)
	}
	if got == "" {
		t.Fatal("instanceRootFrom returned an EMPTY path with no error — renderRootsFn would exec with an undefined working directory")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("instanceRootFrom = %q, want an absolute path", got)
	}
	if mustEval(t, got) != mustEval(t, root) {
		t.Errorf("instanceRootFrom = %s, want %s", got, root)
	}

	// A directory with no landingzone.yaml above it terminates at the filesystem
	// root with an error, rather than reporting a bogus "found".
	bare := t.TempDir()
	if got, err := instanceRootFrom(bare); err == nil {
		t.Errorf("instanceRootFrom(%s) = %q with no error; nothing up-tree holds landingzone.yaml", bare, got)
	}
}

// tfInitWithRetry is the other bounded loop: it must stop at tfInitAttempts and
// hand back the LAST error, sleeping between tries only.
func TestTfInitWithRetry_GivesUpAfterTheAttemptBudget(t *testing.T) {
	prevStream, prevSleep := TfInitStream, tfInitSleep
	t.Cleanup(func() { TfInitStream, tfInitSleep = prevStream, prevSleep })

	var calls int
	var slept []time.Duration
	tfInitSleep = func(d time.Duration) { slept = append(slept, d) }
	TfInitStream = func(...string) error { calls++; return errors.New("blip") }

	if err := tfInitWithRetry(testDeps(t), "-backend-config=bucket=b"); err == nil {
		t.Fatal("init failing every attempt must return an error")
	}
	if calls != tfInitAttempts {
		t.Errorf("ran init %d times, want %d", calls, tfInitAttempts)
	}
	if len(slept) != tfInitAttempts-1 {
		t.Errorf("slept %d times, want %d (no sleep after the last attempt)", len(slept), tfInitAttempts-1)
	}
	for i, d := range slept {
		if d <= 0 {
			t.Errorf("backoff %d = %s, want a positive delay", i, d)
		}
	}

	// A blip that clears is not a failure: init is idempotent, so one retry wins.
	calls = 0
	TfInitStream = func(...string) error {
		calls++
		if calls == 1 {
			return errors.New("blip")
		}
		return nil
	}
	if err := tfInitWithRetry(testDeps(t)); err != nil || calls != 2 {
		t.Errorf("blip-then-ok: err=%v after %d attempts, want nil after 2", err, calls)
	}
}
