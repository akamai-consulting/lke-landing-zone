package openbao

// execseams_test.go — the exec seams, from this package's side.
//
// FOUR SEAMS AT THREE LEVELS live in internal/baoread and they are NOT
// interchangeable; see that package's exec.go header. Everything in this package
// calls baoread.ExecFn — the RESILIENT one. Stubbing baoread.ExecRaw here would
// leave the retry wrapper live and multiply each stubbed call by the backoff
// count; stubbing kubectlprobe.Exec would reach only findLeaderPod and the
// context probe. Neither mistake errors.

import (
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
)

func withBaoExec(t *testing.T, fn func(pod, token, stdin string, args ...string) (string, string, error)) {
	t.Helper()
	orig := baoread.ExecFn
	baoread.ExecFn = fn
	t.Cleanup(func() { baoread.ExecFn = orig })
}

func withBaoSleep(t *testing.T) *int {
	t.Helper()
	orig := baoread.Sleep
	n := new(int)
	baoread.Sleep = func(time.Duration) { *n++ }
	t.Cleanup(func() { baoread.Sleep = orig })
	return n
}
