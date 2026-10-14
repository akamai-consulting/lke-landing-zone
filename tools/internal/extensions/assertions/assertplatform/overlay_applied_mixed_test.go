package assertplatform

// overlay_applied_mixed_test.go covers the arm that decides whether a lane with
// nothing examined can be GREEN.
//
// `if len(absent) > 0 && !anyUnreadable(verdicts)` is the whole difference
// between "this instance does not run the component" (a legitimate pass) and
// "this run could not tell" (a false green). Deleting the `&& !anyUnreadable`
// clause — the entire guard — left all 146 packages green, so the load-bearing
// half of that fix was proven by nothing.
//
// AND IT IS A MIXED RUN THAT REACHES IT. readLiveObject is called once PER ROW,
// and every mapped row today points at the SAME StatefulSet, so a transient fault
// part-way through a run — a 5xx, an i/o timeout, a webhook blip — answers some
// rows "absent" and leaves one unanswerable. No existing test builds that shape:
// they stub one answer for every row, so `absent` and `unreadable` are never both
// non-empty and the clause is never the thing that decides.

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// overlayRun is a run's error AND what it printed. captureOverlayReport next door
// t.Fatalf's on a non-nil error, so it cannot express a run that MUST fail — which
// is every case in this file.
type overlayRun struct {
	err    error
	report string
}

func captureOverlayRun(t *testing.T, fn func() error) overlayRun {
	t.Helper()
	prev := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = prev })
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	runErr := fn()
	_ = w.Close()
	os.Stdout = prev
	return overlayRun{err: runErr, report: <-done}
}

// withPerRowLiveObject answers the Nth read differently from the rest, which is
// what a fault part-way through a run actually looks like.
func withPerRowLiveObject(t *testing.T, unreadableOn int) {
	t.Helper()
	prev := readLiveObject
	n := 0
	readLiveObject = func(string, string, string) ([]byte, bool, bool, string) {
		n++
		if n == unreadableOn {
			return nil, false, false, "Error from server: etcdserver: request timed out"
		}
		return nil, true, true, `statefulsets.apps "loki-ingester" not found`
	}
	t.Cleanup(func() { readLiveObject = prev })
}

func TestARunWhereSomeRowsAreAbsentAndOneIsUnreadableIsNotGreen(t *testing.T) {
	withPerRowLiveObject(t, 2)
	withOwnerExists(t, false) // the app is not installed — the ONLY reason an absent object passes
	withDryRun(t, "", true)

	got := captureOverlayRun(t, assertOverlayApplied)
	if got.err == nil {
		t.Fatalf("a run that could not read one of its objects reported success. Every row points at "+
			"the same StatefulSet, so this is one transient fault mid-run — and the lane said "+
			"\"nothing to check here\" about a cluster it had not finished asking.\nreport:\n%s", got.report)
	}
	if strings.Contains(got.report, "Nothing to check on this cluster") {
		t.Errorf("the lane printed its does-not-run-this-component message while one object was "+
			"unreadable — that message is a claim about the cluster, made without the evidence "+
			"for it.\nreport:\n%s", got.report)
	}
}

func TestARunWhereEveryRowIsGenuinelyAbsentIsStillGreen(t *testing.T) {
	// The control, and it is not optional: without it the test above passes with the
	// clause hard-wired to fail, which turns a lane the suite runs for everyone into
	// the "gate nobody can turn green" the field map's own exemption table argues
	// against.
	withLiveObject(t, "", true, true)
	withOwnerExists(t, false)
	withDryRun(t, "", true)

	got := captureOverlayRun(t, assertOverlayApplied)
	if got.err != nil {
		t.Fatalf("an instance that does not run the component was failed: %v\n%s", got.err, got.report)
	}
	if !strings.Contains(got.report, "Nothing to check on this cluster") {
		t.Errorf("the legitimate pass no longer says why it passed.\nreport:\n%s", got.report)
	}
}
