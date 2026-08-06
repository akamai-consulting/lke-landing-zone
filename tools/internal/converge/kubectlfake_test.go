package converge

// kubectlfake_test.go — a COPY of package main's fake kubectl harness
// (ci_kyverno_test.go), not an export of it.
//
// Test fixtures are the one thing worth duplicating across an extraction
// boundary. Exporting a fake from package main would make an extension's tests
// depend on the CLI they were extracted out of, which is the dependency the
// extraction exists to remove; and a shared fixture that both sides evolve is how
// one package's test data silently starts constraining another's.
//
// If this drifts from the original, that is fine — they are testing different
// things.

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// fakeKubectl scripts kubectl responses keyed by a substring of the joined argv,
// and records the calls made.
type fakeKubectl struct {
	responses []kubectlRule
	calls     []string
}

type kubectlRule struct {
	match string // substring that must appear in the joined args
	out   string
	ok    bool
}

func (f *fakeKubectl) run(args ...string) (string, bool) {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, joined)
	for _, r := range f.responses {
		if strings.Contains(joined, r.match) {
			return r.out, r.ok
		}
	}
	return "", true // default: success, no output
}

func (f *fakeKubectl) called(substr string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// fakeClock advances a fixed step each time now() is read so deadline loops
// terminate without real sleeping.
func fakeClock(step time.Duration) (func() time.Time, *time.Duration) {
	base := time.Unix(1_700_000_000, 0)
	elapsed := new(time.Duration)
	now := func() time.Time {
		t := base.Add(*elapsed)
		*elapsed += step
		return t
	}
	return now, elapsed
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
	// Restore BEFORE reading: the copy goroutine finishes only on EOF, and EOF
	// arrives only once the write end is closed.
	*target = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// withExecOutput swaps this package's Exec seam AND kubectlprobe's for one test.
// Both, because converge reads the cluster two ways and stubbing one leaves the
// other shelling out for real — which, as the cluster-access extraction
// established, is a hang rather than a failure.
func withExecOutput(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig, origProbe := deps.Exec, kubectlprobe.Exec
	deps.Exec = fn
	kubectlprobe.Exec = fn
	t.Cleanup(func() { deps.Exec, kubectlprobe.Exec = orig, origProbe })
}

// TestMain zeroes the probe retry delay for the whole package.
//
// WITHOUT IT EVERY TEST HERE PAYS 6 REAL SECONDS. internal/kubectlprobe retries an
// unanswerable kubectl call three times with a 3s gap, and these tests stub kubectl
// with errors constantly — so the suite took 568 seconds and then started tripping
// the 300s CI timeout outright.
//
// package main had this line in its own TestMain (kubectl_probe_test.go) and it
// did not travel with the code, because a guard wired into one package's TestMain
// is invisible to the files being moved. That is the SECOND time this exact shape
// has cost an extraction: internal/clusteraccess lost the re-exec half of the same
// TestMain and hung outright. Any extraction that moves code touching
// internal/kubectlprobe needs this line.
func TestMain(m *testing.M) {
	kubectlprobe.Delay = 0
	os.Exit(m.Run())
}
