package releasepublish

import (
	"errors"
	"testing"
	"time"
)

// TestPinGHRetryBacksOff pins the retry CADENCE, not just the retry count. The
// retry exists to ride out a live GitHub API incident (a 503 on the first
// Instantiate query has killed a release-e2e dispatch at minute one) — three
// attempts fired back-to-back inside a few milliseconds retry straight through
// the same outage and buy nothing, so the growing gap between them is the part
// that does the work.
func TestPinGHRetryBacksOff(t *testing.T) {
	origGH, origSleep := pinGH, pinSleep
	t.Cleanup(func() { pinGH, pinSleep = origGH, origSleep })

	var slept []time.Duration
	pinSleep = func(d time.Duration) { slept = append(slept, d) }
	pinGH = func(_ string, _ ...string) ([]byte, error) { return nil, errors.New("HTTP 503") }

	if _, err := pinGHRetry("tok", "api", "x"); err == nil {
		t.Fatal("persistent failure must surface")
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second}
	if len(slept) != len(want) {
		t.Fatalf("slept %v, want %v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Fatalf("backoff = %v, want %v (attempt N waits N×5s)", slept, want)
		}
	}
}
