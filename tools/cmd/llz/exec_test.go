package main

import (
	"errors"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/buildpreflight"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/selfupgrade"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// withExecOutput / withLookPath swap the package-level exec seam for the
// duration of a test, restoring the real implementation afterward.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	// ONE SEAM NOW. execOutput is a closure delegating to kubectlprobe.Exec, so
	// swapping the one underneath covers package main and every extracted package
	// at once — this used to have to swap a pair and say why.
	// internal/configreadiness captures its Deps by value, so it still needs
	// reinstalling after the swap.
	origProbe := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	installConfigReadinessDeps()
	t.Cleanup(func() {
		kubectlprobe.Exec = origProbe
		installConfigReadinessDeps()
	})
}

func withLookPath(t *testing.T, fn func(file string) (string, error)) {
	t.Helper()
	// Stub the ONE seam: execLookPath delegates here, so swapping this covers both.
	orig := kubectlprobe.LookPathFn
	kubectlprobe.LookPathFn = fn
	t.Cleanup(func() { kubectlprobe.LookPathFn = orig })
}

func TestGitOut(t *testing.T) {
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "git" {
			t.Errorf("buildpreflight.GitOut shelled out to %q, want git", name)
		}
		return []byte("  deadbeef\n"), nil
	})
	if got := buildpreflight.GitOut("rev-parse", "HEAD"); got != "deadbeef" {
		t.Errorf("buildpreflight.GitOut = %q, want deadbeef (trimmed)", got)
	}

	// Any error yields the empty string.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("boom") })
	if got := buildpreflight.GitOut("status"); got != "" {
		t.Errorf("buildpreflight.GitOut(error) = %q, want empty", got)
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
			t.Errorf("kubectlprobe.Out shelled out to %q, want kubectl", name)
		}
		return []byte("raw-output"), nil
	})
	got, err := kubectlprobe.Out("get", "pods")
	if err != nil || got != "raw-output" {
		t.Errorf("kubectlprobe.Out = (%q, %v), want (raw-output, nil)", got, err)
	}
}

func TestHaveToolAndLookable(t *testing.T) {
	withLookPath(t, func(file string) (string, error) { return "/usr/bin/" + file, nil })
	if !haveTool("tflint") {
		t.Error("haveTool(present) = false, want true")
	}
	if !kubectlprobe.Lookable("gh") {
		t.Error("kubectlprobe.Lookable(present) = false, want true")
	}

	withLookPath(t, func(string) (string, error) { return "", errors.New("not found") })
	if haveTool("tflint") {
		t.Error("haveTool(absent) = true, want false")
	}
	if kubectlprobe.Lookable("gh") {
		t.Error("kubectlprobe.Lookable(absent) = true, want false")
	}
}

func TestLatestRelease(t *testing.T) {
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "gh" || len(args) == 0 || args[0] != "release" {
			t.Errorf("selfupgrade.LatestRelease shelled out to %q %v, want gh release ...", name, args)
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
	tag, err := selfupgrade.LatestRelease("akamai/lke-landing-zone")
	if err != nil || tag != "v0.0.37" {
		t.Errorf("selfupgrade.LatestRelease = (%q, %v), want (v0.0.37, nil)", tag, err)
	}

	// Only pre-releases/prefixed tags -> error (no full release to serve).
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return []byte(`[{"tagName":"v1.0.0","isDraft":false,"isPrerelease":true},{"tagName":"llz/v1.0.0","isDraft":false,"isPrerelease":false}]`), nil
	})
	if _, err := selfupgrade.LatestRelease("x"); err == nil {
		t.Error("selfupgrade.LatestRelease(no full release) = nil, want error")
	}
}
