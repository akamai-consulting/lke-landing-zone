package health

import (
	"testing"
	"time"
)

// workloads_mutation_test.go pins both halves of LeaseStale: the non-positive
// duration fallback, and the exact 4× staleness multiplier. Between them they
// decide whether a silently-stopped controller is reported — the existing test
// only samples points far from either boundary.

func leaseNow() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }

// TestLeaseStale_DurationFallback pins WHICH durations fall back to the k8s
// default of 15s. Only a non-positive leaseDurationSeconds does: a zero duration
// must not become a zero threshold (every lease instantly stale), and a positive
// duration must be used as given (a 60s lease is stale at 240s, not at 60s).
func TestLeaseStale_DurationFallback(t *testing.T) {
	now := leaseNow()
	cases := []struct {
		name     string
		age      time.Duration
		duration int
		want     bool
	}{
		// duration 0 -> fallback 15 -> threshold 60s.
		{"absent duration, 30s old", 30 * time.Second, 0, false},
		{"absent duration, 61s old", 61 * time.Second, 0, true},
		// negative duration -> same fallback.
		{"negative duration, 30s old", 30 * time.Second, -5, false},
		{"negative duration, 61s old", 61 * time.Second, -5, true},
		// positive duration -> used as-is (threshold 240s), NOT replaced by 15.
		{"60s duration, 120s old", 120 * time.Second, 60, false},
		{"60s duration, 241s old", 241 * time.Second, 60, true},
		// the smallest positive duration is honoured too (threshold 4s).
		{"1s duration, 3s old", 3 * time.Second, 1, false},
		{"1s duration, 5s old", 5 * time.Second, 1, true},
	}
	for _, c := range cases {
		if got := LeaseStale(now.Add(-c.age), now, c.duration); got != c.want {
			t.Errorf("%s: LeaseStale(age=%v, duration=%d) = %v, want %v", c.name, c.age, c.duration, got, c.want)
		}
	}
}

// TestLeaseStale_ThresholdIsExactlyFourDurations pins the multiplier and the
// strictness of the comparison: a lease renewed exactly 4× ago is NOT yet stale,
// and 3×/5× sit either side of the verdict.
func TestLeaseStale_ThresholdIsExactlyFourDurations(t *testing.T) {
	now := leaseNow()
	const duration = 15 // threshold = 60s
	cases := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"3x duration", 45 * time.Second, false},
		{"just under 4x", 59*time.Second + 999*time.Millisecond, false},
		{"exactly 4x", 60 * time.Second, false},
		{"just over 4x", 60*time.Second + time.Millisecond, true},
		{"5x duration", 75 * time.Second, true},
		{"freshly renewed", 0, false},
	}
	for _, c := range cases {
		if got := LeaseStale(now.Add(-c.age), now, duration); got != c.want {
			t.Errorf("%s: LeaseStale(age=%v, duration=%d) = %v, want %v", c.name, c.age, duration, got, c.want)
		}
	}
}
