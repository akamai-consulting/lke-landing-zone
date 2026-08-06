package assertreconciler

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// TestMain zeroes the probe retry delay for the whole package — see
// internal/converge's for what its absence costs (a suite that went 4s → 568s).
func TestMain(m *testing.M) {
	kubectlprobe.Delay = 0
	os.Exit(m.Run())
}

// testDeps installs implementations that DO THE WORK, and restores the previous
// set afterward. ExecCombined in particular returns identifiable text rather than
// "": it is the diagnostic seam, and an assertion whose diagnostics come back
// empty reports a failure with no evidence attached.
func testDeps(t *testing.T) {
	t.Helper()
	orig := deps
	t.Cleanup(func() { deps = orig })
	deps = Deps{
		Exec:         func(string, ...string) ([]byte, error) { return nil, nil },
		ExecCombined: func(name string, _ ...string) string { return "no stub installed for " + name },
		WithPrometheus: func(string, func(func(string) ([]byte, error)) error) error {
			return nil
		},
		FirewallConfigMapName: "llz-firewall-config",
	}
	origProbe := kubectlprobe.Exec
	kubectlprobe.Exec = func(string, ...string) ([]byte, error) { return nil, nil }
	t.Cleanup(func() { kubectlprobe.Exec = origProbe })
}

// withExec swaps the raw shell-out seam AND kubectlprobe's. Both, because this
// package reads the cluster two ways; stubbing one leaves the other reaching for a
// real cluster, which earlier extractions established is a hang rather than a
// failure.
func withExec(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	origExec, origProbe := deps.Exec, kubectlprobe.Exec
	deps.Exec = fn
	kubectlprobe.Exec = fn
	t.Cleanup(func() { deps.Exec, kubectlprobe.Exec = origExec, origProbe })
}

func captureStdout(t *testing.T, fn func()) string { t.Helper(); return capture(t, &os.Stdout, fn) }
func captureStderr(t *testing.T, fn func()) string { t.Helper(); return capture(t, &os.Stderr, fn) }

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
	*target = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}
