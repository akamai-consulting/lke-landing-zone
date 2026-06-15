package main

import (
	"errors"
	"testing"
)

// withExecOutput / withLookPath swap the package-level exec seam for the
// duration of a test, restoring the real implementation afterward.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := execOutput
	execOutput = fn
	t.Cleanup(func() { execOutput = orig })
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

func TestCommitReachable(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, nil })
	if !commitReachable("abc123") {
		t.Error("commitReachable = false on success, want true")
	}
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("unknown rev") })
	if commitReachable("abc123") {
		t.Error("commitReachable = true on error, want false")
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

func TestListArgoApps(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return []byte(`{"items":[
			{"metadata":{"name":"app1"},"status":{"sync":{"Status":"Synced"},"health":{"Status":"Healthy"}}},
			{"metadata":{"name":"app2"},"status":{"sync":{"Status":"OutOfSync"},"health":{"Status":"Degraded"}}}
		]}`), nil
	})
	apps, err := listArgoApps()
	if err != nil || len(apps) != 2 {
		t.Fatalf("listArgoApps = (%d apps, %v), want 2", len(apps), err)
	}
	if apps[0].Name != "app1" || !apps[0].healthy() {
		t.Errorf("app1 = %+v, want Synced+Healthy", apps[0])
	}
	if apps[1].healthy() {
		t.Errorf("app2 = %+v, want not healthy", apps[1])
	}
}

func TestListArgoAppsErrors(t *testing.T) {
	// Transport error propagates.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("no cluster") })
	if _, err := listArgoApps(); err == nil {
		t.Error("listArgoApps(exec error) = nil, want error")
	}
	// Malformed JSON is a parse error.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte("not json"), nil })
	if _, err := listArgoApps(); err == nil {
		t.Error("listArgoApps(bad json) = nil, want error")
	}
}

func TestLatestRelease(t *testing.T) {
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "gh" || len(args) == 0 || args[0] != "release" {
			t.Errorf("latestRelease shelled out to %q %v, want gh release ...", name, args)
		}
		// Bare vX.Y.Z full releases are the CLI track. Pre-releases (unpromoted e2e
		// candidates), drafts, and prefixed legacy/module tags are all ignored — so
		// v0.3.0, though highest, is skipped because it is still a pre-release.
		return []byte(`[` +
			`{"tagName":"v0.1.0","isDraft":false,"isPrerelease":false},` +
			`{"tagName":"v0.2.0","isDraft":false,"isPrerelease":false},` +
			`{"tagName":"v0.3.0","isDraft":false,"isPrerelease":true},` +
			`{"tagName":"v0.4.0","isDraft":true,"isPrerelease":false},` +
			`{"tagName":"llz/v0.9.0","isDraft":false,"isPrerelease":false}]`), nil
	})
	tag, err := latestRelease("akamai/lke-landing-zone")
	if err != nil || tag != "v0.2.0" {
		t.Errorf("latestRelease = (%q, %v), want (v0.2.0, nil)", tag, err)
	}

	// Only pre-releases/prefixed tags -> error (no full release to serve).
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return []byte(`[{"tagName":"v1.0.0","isDraft":false,"isPrerelease":true},{"tagName":"llz/v1.0.0","isDraft":false,"isPrerelease":false}]`), nil
	})
	if _, err := latestRelease("x"); err == nil {
		t.Error("latestRelease(no full release) = nil, want error")
	}
}
