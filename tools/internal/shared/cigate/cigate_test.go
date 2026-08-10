package cigate

// The one test that is unambiguously ABOUT this package: PollUntil's attempt cap.
//
// The rest of package main's ci_shared tests exercise cigate THROUGH the verbs
// that call it (assert-argo-app, kyverno, bootstrap-cluster), so `go test
// -coverprofile` credits them to package main. That is why this package's floor
// is low and not zero — read it as "its tests are elsewhere", which the Makefile
// header asks to be said explicitly.

import (
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// TestPollUntilCannotSpinOnAStuckClock pins the attempt cap. The deadline check
// alone is defeated by a REAL clock paired with a no-op sleep — the exact
// combination every bootstrapDeps fake uses — and the result is a busy-spin for the
// full timeout, which reads as a deadlock rather than a clock bug.
func TestPollUntilCannotSpinOnAStuckClock(t *testing.T) {
	t.Run("stuck clock, no-op sleep: still terminates", func(t *testing.T) {
		calls := 0
		stuck := time.Now()
		start := time.Now()
		ok := PollUntil(
			func() time.Time { return stuck }, // never advances
			func(time.Duration) {},            // never waits
			10*time.Minute, 10*time.Second,
			func() bool { calls++; return false },
		)
		if ok {
			t.Fatal("cond never succeeded, want false")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("took %s — spinning, not attempt-bounded", elapsed)
		}
		// 10m/10s + 1. Enough to be a real budget, bounded enough to end.
		if want := 61; calls != want {
			t.Errorf("cond called %d times, want %d (timeout/interval + 1)", calls, want)
		}
	})

	t.Run("succeeds on the first probe without sleeping", func(t *testing.T) {
		slept := 0
		if !PollUntil(time.Now, func(time.Duration) { slept++ }, time.Minute, time.Second, func() bool { return true }) {
			t.Fatal("want true")
		}
		if slept != 0 {
			t.Errorf("slept %d times before the first probe succeeded", slept)
		}
	})

	t.Run("zero interval does not divide by zero", func(t *testing.T) {
		if PollUntil(time.Now, func(time.Duration) {}, time.Minute, 0, func() bool { return false }) {
			t.Fatal("want false")
		}
	})
}

// W() and GrantedBy are the two places a Deps acquires (or fails to acquire) the
// ability to mutate, so both need to be exact.
func TestWIsNilSafeAndDeniedByDefault(t *testing.T) {
	// A struct LITERAL — which every test in this tree builds — leaves Writer nil.
	// The whole point of the denied handle is that an ungranted capability fails as
	// a refusal you can read, and a nil interface method call is a panic instead.
	var d Deps
	if d.W() == nil {
		t.Fatal("W() returned nil")
	}
	if err := d.W().PermitsWrite(); err == nil {
		t.Error("a zero Deps could mutate — the default must be refusal")
	}
	if _, err := d.W().Delete("ns", "job", "x"); err == nil {
		t.Error("a zero Deps deleted through the Writer")
	}
}

func TestGrantedByScopesToTheBinding(t *testing.T) {
	readOnly := extension.Binding{
		Kind: extension.Assertion, State: extension.Converged,
		Grants: []extension.Grant{extension.ClusterRead},
	}
	if err := NewDepsFor("").GrantedBy(readOnly).W().PermitsWrite(); err == nil {
		t.Error("GrantedBy handed cluster-write to a binding that declared only cluster-read")
	}

	writes := extension.Binding{
		Kind: extension.Transition, State: extension.Converged,
		Grants: []extension.Grant{extension.ClusterRead, extension.ClusterWrite},
	}
	if err := NewDepsFor("").GrantedBy(writes).W().PermitsWrite(); err != nil {
		t.Errorf("GrantedBy refused a binding that declared cluster-write: %v", err)
	}
}

// NewDepsFor must NOT hand out a writer. Granting is an explicit act at the
// construction site, which is what makes `grep -rn GrantedBy` a complete list of
// every place in the tree that can mutate a cluster.
func TestNewDepsForIsDeniedUntilGranted(t *testing.T) {
	if err := NewDepsFor("").W().PermitsWrite(); err == nil {
		t.Error("NewDepsFor returned a Deps that can mutate without any binding being named")
	}
}
