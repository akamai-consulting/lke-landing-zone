package onboard

// execstub_test.go — the shell-out stub for tests that came from package main.
// kubectlprobe.Exec is the one seam; this package reaches it through a closure.

import (
	"io"
	"os"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	prev := kubectlprobe.Exec
	kubectlprobe.Exec = fn
	t.Cleanup(func() { kubectlprobe.Exec = prev })
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	defer func() { os.Stdout = orig }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}
