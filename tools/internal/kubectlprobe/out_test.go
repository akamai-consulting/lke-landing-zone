package kubectlprobe_test

// Out and Lookable arrived from cmd/llz with no tests of their own — four callers
// stubbed the seams underneath them, so the two functions themselves were
// exercised by nothing.

import (
	"errors"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

func TestOutReturnsBothOutputAndError(t *testing.T) {
	orig := kubectlprobe.Exec
	t.Cleanup(func() { kubectlprobe.Exec = orig })

	var gotName string
	var gotArgs []string
	kubectlprobe.Exec = func(name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return []byte("partial output"), errors.New("exit 1")
	}
	out, err := kubectlprobe.Out("get", "pods")

	if gotName != "kubectl" {
		t.Errorf("binary = %q, want kubectl — that is the whole point of Out", gotName)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "get" {
		t.Errorf("args = %v, want [get pods]", gotArgs)
	}
	// A failing kubectl still writes to stdout, and callers print it. Dropping the
	// output on error would turn a diagnosable failure into a bare exit code.
	if out != "partial output" {
		t.Errorf("out = %q, want the output preserved alongside the error", out)
	}
	if err == nil {
		t.Error("the error must survive too")
	}
}

func TestLookableGoesThroughTheSeam(t *testing.T) {
	orig := kubectlprobe.LookPathFn
	t.Cleanup(func() { kubectlprobe.LookPathFn = orig })

	kubectlprobe.LookPathFn = func(string) (string, error) { return "/usr/bin/thing", nil }
	if !kubectlprobe.Lookable("thing") {
		t.Error("a resolvable binary must be Lookable")
	}
	kubectlprobe.LookPathFn = func(string) (string, error) { return "", errors.New("not found") }
	if kubectlprobe.Lookable("thing") {
		t.Error("an unresolvable binary must not be — and this must not consult the real PATH, " +
			"which is what made the seam necessary")
	}
}
