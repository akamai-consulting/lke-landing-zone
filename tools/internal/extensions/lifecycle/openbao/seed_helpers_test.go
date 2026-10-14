package openbao

// fixtures the moved tests need.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
)

// assertArgoAppDeps builds seam deps: kubectl answers from the script (keyed
// by joined args prefix), a fake clock advanced by sleep.
func assertArgoAppDeps(t *testing.T, script func(call int, args []string) (string, bool)) (cigate.Deps, *int) {
	t.Helper()
	now := time.Unix(0, 0)
	calls := 0
	kube := func(args ...string) (string, bool) {
		calls++
		return script(calls, args)
	}
	return cigate.Deps{
		Kubectl: kube,
		// Granted from this extension's own binding and routed to the same fake, so
		// the hard-refresh path is exercised rather than refused — and so a test
		// cannot hand itself a write openbao-seed did not declare.
		Writer: capability.WithExec(Extension().Bindings[0],
			func(_ string, args ...string) ([]byte, error) { out, _ := kube(args...); return []byte(out), nil },
			func(_ string, args ...string) string { out, _ := kube(args...); return out }).Writer,
		Now: func() time.Time { return now },
		Sleep: func(d time.Duration) {
			if d <= 0 {
				d = time.Hour // never freeze: a zero interval must fail an assertion, not hang
			}
			now = now.Add(d)
		},
	}, &calls
}

// withSeedKubectlApply records what the seal-key path WRITES, without a cluster.
//
// Both seams, because the seal key moved from apply to create (apply is an upsert
// and two concurrent seeds would both write) while the CA path still applies.
// Stubbing one and leaving the other live is how a test starts shelling out.
func withSeedKubectlApply(t *testing.T) *string {
	t.Helper()
	var applied string
	prevApply, prevCreate := KubectlApply, KubectlCreate
	KubectlApply = func(manifest string) error { applied = manifest; return nil }
	KubectlCreate = func(manifest string) (string, error) { applied = manifest; return "", nil }
	t.Cleanup(func() { KubectlApply, KubectlCreate = prevApply, prevCreate })
	return &applied
}

// withSeedKubectlCreateConflict makes the seal-key create lose the race, which is
// the answer `apply` could never produce.
func withSeedKubectlCreateConflict(t *testing.T) *int {
	t.Helper()
	n := new(int)
	prev := KubectlCreate
	KubectlCreate = func(string) (string, error) {
		*n++
		return `Error from server (AlreadyExists): secrets "openbao-unseal-key" already exists`,
			errString("exit 1")
	}
	t.Cleanup(func() { KubectlCreate = prev })
	return n
}

type errString string

func (e errString) Error() string { return string(e) }

// withSetGitHubSecret records the escrow write.
func withGHSetSecretErr(t *testing.T, err error) *[]string {
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

// withGHAEnvFile captures $GITHUB_ENV writes; returns the path.
func withGHAEnvFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gha-env")
	t.Setenv("GITHUB_ENV", p)
	return p
}

// writeFileMkdir writes content at path, creating parent dirs (mustWrite does not).
func ghaEnvContains(t *testing.T, path, want string) bool {
	t.Helper()
	b, _ := os.ReadFile(path)
	return strings.Contains(string(b), want)
}

// withKubeExec swaps the seam the k8s: field source actually reads through.
//
// It is kube.Exec, not kubectlprobe.Exec — resolveSeedFields takes
// kube.SecretFieldOf. Stubbing the wrong one is silent: the field reads as ABSENT,
// the seeder reports missing inputs and exits 0 by design, and the test then finds
// zero writes with no error to explain them. The seam a fixture swaps has to be
// the one the code path takes, which is the same lesson as the double-seam trap
// from the other direction.
func withKubeExec(t *testing.T, h func(args string) ([]byte, error)) {
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

// withSeedBaoExec stubs ALL THREE bao seams a seed path touches: the stdin-carrying
// exec it names, the plain Exec the seeded-path CHECK reads through, and KVPut.
//
// Stubbing only the first is the double-seam trap seen repeatedly
// here. Delegating Exec to ExecStdin keeps one fake behaviour.
func withSeedBaoExec(t *testing.T, fn func(token, stdin string, args ...string) (string, string, error)) {
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
