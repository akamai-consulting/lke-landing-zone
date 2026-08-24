package healthsla

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// td is the Deps under test. The stub helpers below mutate it, and each test
// starts from testDeps(t), which installs implementations that ACTUALLY WORK
// rather than no-ops.
//
// That distinction is not stylistic. Two earlier extractions shipped a fixture
// stubbed to return a zero value — teardown's Summary and objenc's SecretField —
// and in both cases every assertion downstream ran against nothing and passed.
// Summary here appends to a real file, so a test asserting on summary contents
// is asserting on something.
var td Deps

// ensureDeps installs the baseline exactly once per test, so a stub helper can
// be called before or after it without the baseline wiping the stub. Ordering
// dependence between fixtures is its own bug class — this removes it rather than
// documenting it.
func ensureDeps(t *testing.T) {
	t.Helper()
	if !depsInstalled {
		testDeps(t)
	}
}

var depsInstalled bool

func testDeps(t *testing.T) {
	t.Helper()
	depsInstalled = true
	t.Cleanup(func() { depsInstalled = false })
	td = Deps{
		Summary: realAppend,
		BaoExec: func(string, string, string, ...string) (string, string, error) {
			return "", "", fmt.Errorf("no bao stub installed")
		},
		Reachable: func() bool { return true },
	}
	// The probes hold their own seam; leaving it live would shell out for real.
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = func(string, ...string) ([]byte, error) { return nil, fmt.Errorf("no kubectl stub installed") }
	t.Cleanup(func() { kubectlprobe.Exec = orig })
}

// realAppend is a genuine GITHUB_STEP_SUMMARY append — the same contract
// package main's appendGHAFile has. Tests read the file back.
func realAppend(envVar string, lines ...string) error {
	path := os.Getenv(envVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}

// stubKubectl drives the classified probes from canned output. It drove the
// direct Deps.Exec seam too until #483 retired the check that owned it — this
// extension now reads the cluster only through internal/kubectlprobe.
func stubKubectl(t *testing.T, fn func(args []string) ([]byte, error)) {
	t.Helper()
	ensureDeps(t)
	wrapped := func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		return fn(args)
	}
	td.Reachable = func() bool {
		_, err := wrapped("kubectl", "version", "--request-timeout=10s")
		return err == nil
	}
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = wrapped
	t.Cleanup(func() { kubectlprobe.Exec = orig })
}

// stubBaoExec replaces the RESILIENT bao exec seam for one test. The readiness
// check reads seal state through this (not a bare kubectl exec) so that
// documented transient failures — konnectivity "No agent available" and friends —
// retry instead of being reported as a sealed pod.
// STDOUT IS FORWARDED EVEN WHEN THE STUB ERRORS. Discarding it made the real
// sealed shape — valid JSON on stdout, exit 2 — inexpressible through this
// helper, so every sealed test written with it had to use `(json, nil)`, a shape
// the real exec never produces. That is precisely what let the "sealed reads as
// UNKNOWN" defect survive a test suite that appeared to cover it.
func stubBaoExec(t *testing.T, fn func(pod string, args []string) (string, error)) {
	t.Helper()
	ensureDeps(t)
	td.BaoExec = func(pod, _, _ string, args ...string) (string, string, error) {
		out, err := fn(pod, args)
		if err != nil {
			return out, err.Error(), err
		}
		return out, "", nil
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return capture(t, &os.Stdout, fn)
}

func capture(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := *target
	*target = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	// Restore BEFORE reading: the copy goroutine only finishes on EOF, and EOF
	// only arrives once the write end is closed.
	*target = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}
