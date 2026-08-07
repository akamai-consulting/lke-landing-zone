package baoread

// execoutput_test.go — the local withExecOutput.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// withExecOutput stubs THE SEAM DumpDiagnostics ACTUALLY TAKES.
//
// ExecPod does not route through kubectlprobe: it builds its own exec.Command so
// it can attach stdin, which kubectlprobe.Exec has no signature for. The only
// caller in this package that goes through kubectlprobe is DumpDiagnostics, and
// that is what these tests exercise. Stubbing the wrong one of the two would not
// error — it would leave the test passing against a live `kubectl`.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = orig })
}
