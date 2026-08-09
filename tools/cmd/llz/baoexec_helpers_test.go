package main

// baoexec_helpers_test.go — the OpenBao exec seams, from package main's side.
//
// Nine test files here still drive the exec layer that now lives in
// internal/baoread; these three helpers moved WITH it and had to be re-declared
// rather than exported, because a `with…(t *testing.T, …)` helper in a shipped
// package is production code that imports testing.

import (
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
)

// withBaoExec swaps the RESILIENT entry point — the one every caller in this
// package reaches for. Stubbing baoread.ExecRaw instead would leave the retry
// wrapper live and silently multiply each stubbed call by the backoff count.
func withBaoExec(t *testing.T, fn func(pod, token, stdin string, args ...string) (string, string, error)) {
	t.Helper()
	orig := baoread.ExecFn
	baoread.ExecFn = fn
	t.Cleanup(func() { baoread.ExecFn = orig })
}

// withBaoExecRaw swaps the UNRETRIED primitive, for the tests that are about the
// retry wrapper itself.
func withBaoExecRaw(t *testing.T, fn func(pod, token, stdin string, args ...string) (string, string, error)) {
	t.Helper()
	orig := baoread.ExecRaw
	baoread.ExecRaw = fn
	t.Cleanup(func() { baoread.ExecRaw = orig })
}

// withBaoSleep makes poll waits instantaneous while counting them.
func withBaoSleep(t *testing.T) *int {
	t.Helper()
	orig := baoread.Sleep
	n := new(int)
	baoread.Sleep = func(time.Duration) { *n++ }
	t.Cleanup(func() { baoread.Sleep = orig })
	return n
}
