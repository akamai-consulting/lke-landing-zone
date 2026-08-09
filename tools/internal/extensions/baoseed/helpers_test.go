package baoseed

// fixtures the moved tests need.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// assertArgoAppDeps builds seam deps: kubectl answers from the script (keyed
// by joined args prefix), a fake clock advanced by sleep.
func assertArgoAppDeps(t *testing.T, script func(call int, args []string) (string, bool)) (cigate.Deps, *int) {
	t.Helper()
	now := time.Unix(0, 0)
	calls := 0
	return cigate.Deps{
		Kubectl: func(args ...string) (string, bool) {
			calls++
			return script(calls, args)
		},
		Now: func() time.Time { return now },
		Sleep: func(d time.Duration) {
			if d <= 0 {
				d = time.Hour // never freeze: a zero interval must fail an assertion, not hang
			}
			now = now.Add(d)
		},
	}, &calls
}

// withExecOutput swaps the kubectl seam THIS package reaches through.
//
// Package main's helper of the same name also reinstalls configreadiness's
// capabilities — irrelevant here, and copying it wholesale drags an unrelated Deps
// install across the boundary to satisfy a name. FOURTH package to need this
// correction; the copier keeps finding the wrong donor because the name matches.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = prev })
}

// withKubectlApply records what the seal-key path applies, without a cluster.
func withKubectlApply(t *testing.T) *string {
	t.Helper()
	var applied string
	prev := KubectlApply
	KubectlApply = func(manifest string) error { applied = manifest; return nil }
	t.Cleanup(func() { KubectlApply = prev })
	return &applied
}

// withSetGitHubSecret records the escrow write.
func withGHSetSecret(t *testing.T, err error) *[]string {
	t.Helper()
	var got []string
	prev := SetGitHubSecret
	SetGitHubSecret = func(name, env, value string) error {
		if err != nil {
			return err
		}
		got = append(got, name+"@"+env+"="+value)
		return nil
	}
	t.Cleanup(func() { SetGitHubSecret = prev })
	return &got
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote — these helpers print a human report we don't want in test output.
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
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// withGHAEnvFile captures $GITHUB_ENV writes; returns the path.
func withGHAEnvFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gha-env")
	t.Setenv("GITHUB_ENV", p)
	return p
}

// captureStderr mirrors captureStdout for the os.Stderr path (the remediation /
// warning printers write there).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// writeFileMkdir writes content at path, creating parent dirs (mustWrite does not).
func ghaEnvContains(t *testing.T, path, want string) bool {
	t.Helper()
	b, _ := os.ReadFile(path)
	return strings.Contains(string(b), want)
}

// withKubectl swaps the seam the k8s: field source actually reads through.
//
// It is kube.Exec, not kubectlprobe.Exec — resolveSeedFields takes
// kube.SecretFieldOf. Stubbing the wrong one is silent: the field reads as ABSENT,
// the seeder reports missing inputs and exits 0 by design, and the test then finds
// zero writes with no error to explain them. The seam a fixture swaps has to be
// the one the code path takes, which is the same lesson as the double-seam trap
// from the other direction.
func withKubectl(t *testing.T, h func(args string) ([]byte, error)) {
	t.Helper()
	prev := kube.Exec
	kube.Exec = func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			return nil, errors.New("unexpected command " + name)
		}
		return h(strings.Join(args, " "))
	}
	t.Cleanup(func() { kube.Exec = prev })
}

// items wraps item JSON blobs into a kubectl list response.

// withBaoExec stubs ALL THREE bao seams a seed path touches: the stdin-carrying
// exec it names, the plain Exec the seeded-path CHECK reads through, and KVPut.
//
// Stubbing only the first is the double-seam trap this campaign has now hit six
// times. Delegating Exec to ExecStdin keeps one fake behaviour.
func withBaoExec(t *testing.T, fn func(token, stdin string, args ...string) (string, string, error)) {
	t.Helper()
	prevStdin, prevExec, prevPut := baoread.ExecStdin, baoread.Exec, baoread.KVPut
	baoread.ExecStdin = fn
	baoread.Exec = func(token string, args ...string) (string, string, error) {
		return baoread.ExecStdin(token, "", args...)
	}
	// KVPut delegates to the same fake, so a stub that fails `kv put` fails the
	// WRITE too. Returning nil here made the abort-on-put-failure test pass
	// vacuously: the fake refused the put and the code never saw it.
	baoread.KVPut = func(path string, fields map[string]string) error {
		args := []string{"kv", "put", path}
		for k, v := range fields {
			args = append(args, k+"="+v)
		}
		if _, stderr, err := baoread.ExecStdin("", "", args...); err != nil {
			return errors.New(stderr)
		}
		return nil
	}
	t.Cleanup(func() {
		baoread.ExecStdin, baoread.Exec, baoread.KVPut = prevStdin, prevExec, prevPut
	})
}
