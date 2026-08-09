package assertplatform

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// TestMain zeroes the probe retry delay for the whole package.
//
// THIS LINE IS NOT OPTIONAL and its absence does not fail, it just makes the
// suite crawl: internal/kubectlprobe retries an unanswerable kubectl call three
// times with a 3s gap, so every stubbed-error test pays six real seconds.
// internal/converge lost this line in extraction and its suite went from 4s to
// 568s before tripping CI's 300s timeout. Any package that moves code touching
// kubectlprobe needs it.
func TestMain(m *testing.M) {
	kubectlprobe.Delay = 0
	os.Exit(m.Run())
}

// testDeps installs implementations that DO THE WORK. ExecCombined in particular:
// it is the diagnostic seam, and an assertion whose diagnostics return "" reports
// a failure with no evidence attached — the vacuous-fixture bug three earlier
// extractions have now paid for.
func testDeps(t *testing.T) {
	t.Helper()
	orig := deps
	t.Cleanup(func() { deps = orig })
	deps = Deps{
		ExecCombined: func(name string, args ...string) string {
			return "no stub installed for " + name
		},
		Exec:     func(string, ...string) ([]byte, error) { return nil, nil },
		LoadSpec: func() (*clusterspec.LandingZone, bool, error) { return nil, false, nil },
	}
	origProbe := kubectlprobe.Exec
	kubectlprobe.Exec = func(string, ...string) ([]byte, error) { return nil, nil }
	t.Cleanup(func() { kubectlprobe.Exec = origProbe })
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
