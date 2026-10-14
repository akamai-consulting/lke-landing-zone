package assertsecrets

// cobra_exec_test.go — the shell-out stub for the moved command's tests.
//
// It swaps kubectlprobe.Exec AND kubectlprobe.Combined, which is the whole seam:
// cobra_deps.go's execOutput/execCombined are closures over them, so replacing
// the pair covers every call the command makes.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = prev })
}
