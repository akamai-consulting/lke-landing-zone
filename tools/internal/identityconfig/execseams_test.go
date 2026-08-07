package identityconfig

// execseams_test.go — which seam each kind of call actually takes.
//
// This package reaches THREE different exec layers and they are not
// interchangeable. Getting this wrong does not error:
//
//	kubectlprobe.Exec   every `kubectl` call — execOutput and kubectlOut both
//	                    delegate to it, so this is the ONE fake for all of them.
//	execCombined        the diagnostics-only exec that ignores exit status. Its
//	                    own package var, because its whole purpose is to return
//	                    text a failing command wrote to stderr.
//	baoread.ExecFn      the in-pod `bao` CLI, retry-wrapped. Stubbing
//	                    baoread.ExecRaw instead leaves the retry live and
//	                    multiplies each stubbed call by the backoff count.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

func withBaoExec(t *testing.T, fn func(pod, token, stdin string, args ...string) (string, string, error)) {
	t.Helper()
	orig := baoread.ExecFn
	baoread.ExecFn = fn
	t.Cleanup(func() { baoread.ExecFn = orig })
}

func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = orig })
}
