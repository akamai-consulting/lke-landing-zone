package upgrade

// clobbered_managed_test.go — the upgrade must NAME the local edits it discards,
// and must keep upgrading when it cannot tell.
//
// The failure this guards is not a crash: it is the advisory quietly reverting to
// a count. "overwrote 31 managed file(s)" and "overwrote 31 managed file(s), one
// of which was yours" are the same green run, and only the second one lets the
// operator recover the change.

import (
	"os"
	"strings"
	"testing"
)

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
// The report is a side effect on a stream, so the stream is what the test reads —
// asserting on a string the function also returned would let the two drift.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// THE POINT OF THE WHOLE CHANGE: the operator's path appears, so they can find
// their change in the diff instead of wondering where it went.
func TestReportClobberedManagedNamesEveryFile(t *testing.T) {
	edited := []string{".github/workflows/llz-terraform.yml", ".tflintrc.hcl"}
	out := captureStderr(t, func() { reportClobberedManaged(edited, false) })

	for _, rel := range edited {
		if !strings.Contains(out, rel) {
			t.Errorf("report does not name %q — the operator cannot find their change:\n%s", rel, out)
		}
	}
	// The remedy has to be in the message. A warning that says a change was lost
	// and not how to get it back sends the reader to the docs mid-upgrade.
	if !strings.Contains(out, "git diff HEAD --") {
		t.Errorf("report carries no recovery command:\n%s", out)
	}
	if !strings.Contains(out, "discarded") {
		t.Errorf("report does not say the change was discarded:\n%s", out)
	}
}

// A dry run has destroyed nothing yet, and saying otherwise would send an
// operator hunting for a change that is still sitting in their tree. It is also
// the run where the warning is worth the most, so it must still appear.
func TestReportClobberedManagedUsesConditionalTenseOnDryRun(t *testing.T) {
	out := captureStderr(t, func() { reportClobberedManaged([]string{".tflintrc.hcl"}, true) })

	if !strings.Contains(out, ".tflintrc.hcl") {
		t.Fatalf("a dry run must still warn — it is the run that can still act:\n%s", out)
	}
	if !strings.Contains(out, "would") {
		t.Errorf("dry run must not claim the edit was already discarded:\n%s", out)
	}
	if strings.Contains(out, "was discarded") {
		t.Errorf("dry run reported a past-tense loss that has not happened:\n%s", out)
	}
}

// SILENCE WHEN THERE IS NOTHING TO SAY. The overwhelming majority of upgrades
// touch no locally-edited file, and a warning block on every one of them is how
// an advisory stops being read.
func TestReportClobberedManagedIsSilentWhenNothingWasEdited(t *testing.T) {
	if out := captureStderr(t, func() { reportClobberedManaged(nil, false) }); out != "" {
		t.Errorf("clean upgrade printed a warning:\n%s", out)
	}
}

// THE ADVISORY MUST NOT BE ABLE TO FAIL THE UPGRADE. managedEditsBefore runs
// before copier against a tree that may have no lock, no manifest, or no read
// permission; every one of those has to degrade to "nothing to report" rather
// than take the upgrade down with it. Run it somewhere with no scaffold at all —
// the harshest version of that — and require it to answer.
func TestManagedEditsBeforeDegradesOutsideAnInstance(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := managedEditsBefore(); len(got) != 0 {
		t.Errorf("outside an instance managedEditsBefore = %v, want empty", got)
	}
}

// The wiring, asserted where it can be: Run captures the edits BEFORE copier and
// reports them after, so the two package vars must both be reachable and the
// report must accept what the capture produces. A signature change that broke the
// pairing would otherwise only show up as a silent advisory in the field.
func TestCaptureAndReportComposeAtTheirRealTypes(t *testing.T) {
	orig := managedEditsBefore
	t.Cleanup(func() { managedEditsBefore = orig })
	managedEditsBefore = func() []string { return []string{"apl-values/values.yaml"} }

	out := captureStderr(t, func() { reportClobberedManaged(managedEditsBefore(), false) })
	if !strings.Contains(out, "apl-values/values.yaml") {
		t.Errorf("the captured set did not reach the report:\n%s", out)
	}
}
