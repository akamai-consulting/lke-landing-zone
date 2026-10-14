package baoread

// Mutation-test gap closure for ci_openbao.go: the retry backoff schedule (the
// budget that decides whether a cold konnectivity warmup fails the whole OpenBao
// bootstrap) and the timeout diagnostics an operator has to debug from.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The backoff is linear (2s per attempt) and CAPPED at 15s, and the whole
// ~5-minute budget across baoExecRetries tries is derived from it. A collapsed
// schedule (0s waits) turns the retry into a tight spin that burns the budget in
// milliseconds and fails the bootstrap on a warmup it was sized to survive; an
// uncapped one blows past the job timeout. Nothing else pinned the actual values.
func TestBaoExecBackoffSchedule(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{7, 14 * time.Second}, // the last uncapped step
		{8, 15 * time.Second}, // 16s would exceed the cap
		{24, 15 * time.Second},
	} {
		if got := baoExecBackoff(tc.attempt); got != tc.want {
			t.Errorf("baoExecBackoff(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}

	// The property the SCAR in the source is about: the total wait across the
	// full retry budget has to be minutes, not milliseconds.
	var total time.Duration
	for a := 1; a < baoExecRetries; a++ {
		total += baoExecBackoff(a)
	}
	if total < 4*time.Minute {
		t.Errorf("total retry budget = %s across %d tries, want >= 4m (a cold konnectivity warmup has been seen to take minutes)", total, baoExecRetries)
	}
}

// The timeout diagnostics ARE the operator's debugging surface. When the log
// fetch succeeds the logs must be printed; when it fails the reason must be
// printed. Reporting one as the other leaves a stuck bootstrap with no evidence.
func TestDumpBaoDiagnosticsLogBranch(t *testing.T) {
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		return "Sealed  true\n", "", nil
	})

	t.Run("logs are printed when the fetch succeeds", func(t *testing.T) {
		withExecOutput(t, func(name string, args ...string) ([]byte, error) {
			if name != "kubectl" || !strings.Contains(strings.Join(args, " "), "logs") {
				t.Errorf("want a kubectl logs fetch, got %q %v", name, args)
			}
			return []byte("panic: static seal key mismatch\n"), nil
		})
		out := captureStdout(t, func() { DumpDiagnostics("platform-openbao-0", true) })
		if !strings.Contains(out, "panic: static seal key mismatch") {
			t.Errorf("the fetched container log must be printed:\n%s", out)
		}
		if strings.Contains(out, "could not fetch logs") {
			t.Errorf("a successful fetch must not report a fetch failure:\n%s", out)
		}
	})

	t.Run("the reason is printed when the fetch fails", func(t *testing.T) {
		withExecOutput(t, func(string, ...string) ([]byte, error) {
			return nil, errors.New("pod not found")
		})
		out := captureStdout(t, func() { DumpDiagnostics("platform-openbao-0", true) })
		if !strings.Contains(out, "could not fetch logs: pod not found") {
			t.Errorf("a failed fetch must say why:\n%s", out)
		}
	})
}
