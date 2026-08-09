package seedspecial

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// Helpers the moved tests use, copied across the new package boundary.

// withGHAEnvFile captures $GITHUB_ENV writes; returns the path.
func withGHAEnvFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gha-env")
	t.Setenv("GITHUB_ENV", p)
	return p
}

// withExecOutput stubs THE SEAM THE CODE PATH ACTUALLY TAKES. execOutput here is
// a plain func delegating to kubectlprobe.Exec (deps.go), so the swappable var is
// kubectlprobe's — stubbing anything else would leave the tests running a real
// `kubectl`.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = orig })
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

func ghaEnvContains(t *testing.T, path, want string) bool {
	t.Helper()
	b, _ := os.ReadFile(path)
	return strings.Contains(string(b), want)
}

func withGHASummaryFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_STEP_SUMMARY", p)
	return p
}

// stubLinode is a COPY of internal/credrotate's test fake — fixtures travel by
// copy rather than being exported from a production package.
type stubLinode struct {
	pats, objkeys []map[string]any
	deleted       []uint64
	verifyErr     error
	patCreates    int
	objCreates    int
}

func (s *stubLinode) ListProfileTokens(context.Context) ([]map[string]any, error) { return s.pats, nil }
func (s *stubLinode) CreateProfileToken(context.Context, string, string, string) (map[string]any, error) {
	s.patCreates++
	return map[string]any{"id": 100 + s.patCreates, "token": "new-pat"}, nil
}
func (s *stubLinode) DeleteProfileToken(_ context.Context, id uint64) error {
	s.deleted = append(s.deleted, id)
	return nil
}
func (s *stubLinode) ListObjectStorageKeys(context.Context) ([]map[string]any, error) {
	return s.objkeys, nil
}
func (s *stubLinode) CreateObjectStorageKeyBuckets(context.Context, string, string, []string, string) (map[string]any, error) {
	s.objCreates++
	// id as json.Number — the only numeric type cli.AsUint64 accepts, mirroring
	// how the real client decodes API responses.
	return map[string]any{"id": jn(200 + s.objCreates), "access_key": "AK", "secret_key": "SK"}, nil
}
func (s *stubLinode) DeleteObjectStorageKey(_ context.Context, id uint64) error {
	s.deleted = append(s.deleted, id)
	return nil
}
func (s *stubLinode) Verify(context.Context) error { return s.verifyErr }

// stubBao is a COPY, same reasoning as stubLinode above.
type stubBao struct{ data map[string]map[string]string }

func (b *stubBao) Get(_ context.Context, path, key string) (string, bool, error) {
	v, ok := b.data[path][key]
	return v, ok, nil
}

// jn: the stub returns Linode IDs as json.Number, matching a real decode.
func jn(i int) json.Number { return json.Number(strconv.Itoa(i)) }

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// insecureClient trusts the httptest server's self-signed certificate. The stub
// speaks TLS because the real OpenBao does, and a client that refused it would
// make every forward test fail for a reason unrelated to what it checks.
func stubBaoSeedKV(t *testing.T, presentField, presentValue string) *[][]string {
	t.Helper()
	var puts [][]string
	prev := baoread.ExecFn
	baoread.ExecFn = func(_, _, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "kv get"):
			if presentField != "" && strings.Contains(joined, "-field="+presentField) {
				return presentValue + "\n", "", nil
			}
			// bao's own words for an absent path. A bare error with no stderr
			// would now mean "the read never got an answer" — which fails the
			// seed closed instead of overwriting a possibly-live credential.
			return "", "No value found at " + lastArg(args), errors.New("exit 2")
		case strings.HasPrefix(joined, "kv put"):
			puts = append(puts, args)
			return "", "", nil
		}
		return "", "unexpected: " + joined, errors.New("unexpected")
	}
	t.Cleanup(func() { baoread.ExecFn = prev })
	return &puts
}

func lastArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1]
}

// withBaoExec swaps the RESILIENT entry point — the one every caller in this
// package reaches for. Stubbing baoread.ExecRaw instead would leave the retry
// wrapper live and silently multiply each stubbed call by the backoff count.
func withBaoExec(t *testing.T, fn func(pod, token, stdin string, args ...string) (string, string, error)) {
	t.Helper()
	orig := baoread.ExecFn
	baoread.ExecFn = fn
	t.Cleanup(func() { baoread.ExecFn = orig })
}

// withKubectl stubs the execOutput seam to answer kubectl invocations via a
// handler keyed on the joined args; non-kubectl shell-outs error. An unstubbed
// kubectl call returns an error, which the section helpers treat as "empty".
func withKubectl(t *testing.T, h func(args string) ([]byte, error)) {
	t.Helper()
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		return h(strings.Join(args, " "))
	})
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
