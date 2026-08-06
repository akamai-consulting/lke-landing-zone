package main

import (
	"errors"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// withExecOutput / withLookPath swap the package-level exec seam for the
// duration of a test, restoring the real implementation afterward.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig, origProbe := execOutput, kubectlprobe.Exec
	execOutput = fn
	// Both extracted packages hold their OWN reference to this seam (wired in the
	// respective init()s), so stubbing only execOutput leaves them shelling out for
	// real. internal/configreadiness reads through the same closure, so reinstalling
	// its Deps after the swap is enough.
	kubectlprobe.Exec = fn
	installConfigReadinessDeps()
	t.Cleanup(func() {
		execOutput, kubectlprobe.Exec = orig, origProbe
		installConfigReadinessDeps()
	})
}

func withLookPath(t *testing.T, fn func(file string) (string, error)) {
	t.Helper()
	orig := execLookPath
	execLookPath = fn
	t.Cleanup(func() { execLookPath = orig })
}

func TestGitOut(t *testing.T) {
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "git" {
			t.Errorf("gitOut shelled out to %q, want git", name)
		}
		return []byte("  deadbeef\n"), nil
	})
	if got := gitOut("rev-parse", "HEAD"); got != "deadbeef" {
		t.Errorf("gitOut = %q, want deadbeef (trimmed)", got)
	}

	// Any error yields the empty string.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("boom") })
	if got := gitOut("status"); got != "" {
		t.Errorf("gitOut(error) = %q, want empty", got)
	}
}

func TestGitOutputPassesDirFlag(t *testing.T) {
	var gotArgs []string
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("ok\n"), nil
	})
	out, err := gitOutput("/work/dir", "rev-parse", "--show-toplevel")
	if err != nil || out != "ok" {
		t.Fatalf("gitOutput = (%q, %v), want (ok, nil)", out, err)
	}
	if len(gotArgs) < 2 || gotArgs[0] != "-C" || gotArgs[1] != "/work/dir" {
		t.Errorf("gitOutput did not pass `-C /work/dir`: %v", gotArgs)
	}
}

func TestKubectlOut(t *testing.T) {
	withExecOutput(t, func(name string, _ ...string) ([]byte, error) {
		if name != "kubectl" {
			t.Errorf("kubectlOut shelled out to %q, want kubectl", name)
		}
		return []byte("raw-output"), nil
	})
	got, err := kubectlOut("get", "pods")
	if err != nil || got != "raw-output" {
		t.Errorf("kubectlOut = (%q, %v), want (raw-output, nil)", got, err)
	}
}

func TestHaveToolAndLookable(t *testing.T) {
	withLookPath(t, func(file string) (string, error) { return "/usr/bin/" + file, nil })
	if !haveTool("tflint") {
		t.Error("haveTool(present) = false, want true")
	}
	if !lookable("gh") {
		t.Error("lookable(present) = false, want true")
	}

	withLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	if haveTool("tflint") {
		t.Error("haveTool(absent) = true, want false")
	}
	if lookable("gh") {
		t.Error("lookable(absent) = true, want false")
	}
}

func TestLatestRelease(t *testing.T) {
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "gh" || len(args) == 0 || args[0] != "release" {
			t.Errorf("latestRelease shelled out to %q %v, want gh release ...", name, args)
		}
		// Bare vX.Y.Z full releases are the CLI track. Every tag ABOVE the expected
		// winner is excluded for a different reason — v0.0.38 is a pre-release (an
		// unpromoted e2e candidate), v0.0.39 is a draft (no git tag exists yet), and
		// llz/v0.0.40 is on the legacy prefixed track — so a filter that dropped any
		// one of the three would return it instead of v0.0.37.
		return []byte(`[` +
			`{"tagName":"v0.0.36","isDraft":false,"isPrerelease":false},` +
			`{"tagName":"v0.0.37","isDraft":false,"isPrerelease":false},` +
			`{"tagName":"v0.0.38","isDraft":false,"isPrerelease":true},` +
			`{"tagName":"v0.0.39","isDraft":true,"isPrerelease":false},` +
			`{"tagName":"llz/v0.0.40","isDraft":false,"isPrerelease":false}]`), nil
	})
	tag, err := latestRelease("akamai/lke-landing-zone")
	if err != nil || tag != "v0.0.37" {
		t.Errorf("latestRelease = (%q, %v), want (v0.0.37, nil)", tag, err)
	}

	// Only pre-releases/prefixed tags -> error (no full release to serve).
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return []byte(`[{"tagName":"v1.0.0","isDraft":false,"isPrerelease":true},{"tagName":"llz/v1.0.0","isDraft":false,"isPrerelease":false}]`), nil
	})
	if _, err := latestRelease("x"); err == nil {
		t.Error("latestRelease(no full release) = nil, want error")
	}
}
