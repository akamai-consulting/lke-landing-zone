package assertreconciler

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// TestMain zeroes the probe retry delay for the whole package — see
// internal/converge's for what its absence costs (a suite that went 4s → 568s).
func TestMain(m *testing.M) {
	kubectlprobe.Delay = 0
	os.Exit(m.Run())
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
