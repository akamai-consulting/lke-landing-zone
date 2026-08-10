package openbao

import (
	"strings"
	"testing"
	"time"
)

// The hard refresh of the wedged parent is throttled to one per 20s, and the
// throttle is inclusive at exactly 20s. Polls are 10s apart, so with the fake
// clock a 15s budget runs t=0, t=10s, t=20s and the refresh must fire on the
// first and last of those — exactly twice. A `>` instead of `>=` misses the
// t=20s poll by a hair (one refresh); a 20*time.Second → 20/time.Second
// arithmetic slip collapses the threshold to zero and refreshes every poll
// (three).
func TestWaitForOpenbaoNamespaceThrottlesTheHardRefresh(t *testing.T) {
	refreshes := 0
	d, _ := assertArgoAppDeps(t, func(_ int, args []string) (string, bool) {
		j := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(j, "get namespace"):
			return "", false // never appears — run to the deadline
		case strings.Contains(j, "annotate") && strings.Contains(j, "refresh=hard"):
			refreshes++
			return "", true
		default: // ComparisonError probe: transient on every poll
			return "failed to list refs: repository not found", true
		}
	})
	if err := WaitForNamespace(d, "llz-openbao", 15*time.Second); err == nil {
		t.Fatal("a namespace that never appears must fail loud at the deadline")
	}
	if refreshes != 2 {
		t.Errorf("hard refreshes = %d, want exactly 2 (t=0 and t=20s under the inclusive 20s throttle, 10s polls)", refreshes)
	}
}

// The deadline error must carry the parent's ComparisonError — that message is
// the only clue why llz-cluster-foundation never created the namespace.
func TestWaitForOpenbaoNamespaceDeadlineCarriesTheComparisonError(t *testing.T) {
	d, _ := assertArgoAppDeps(t, func(_ int, args []string) (string, bool) {
		if strings.HasPrefix(strings.Join(args, " "), "get namespace") {
			return "", false
		}
		// Non-transient (so no refresh loop), but still reportable.
		return "error: kind ExternalSecret not registered", true
	})
	err := WaitForNamespace(d, "llz-openbao", 30*time.Second)
	if err == nil {
		t.Fatal("want a fail-loud deadline error")
	}
	for _, want := range []string{"platform-bootstrap ComparisonError", "kind ExternalSecret not registered"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("deadline error %q missing %q", err, want)
		}
	}
}

// ...and stays quiet about it when there is none, rather than appending an empty
// parenthetical.
func TestWaitForOpenbaoNamespaceDeadlineOmitsAnAbsentComparisonError(t *testing.T) {
	d, _ := assertArgoAppDeps(t, func(_ int, args []string) (string, bool) {
		if strings.HasPrefix(strings.Join(args, " "), "get namespace") {
			return "", false
		}
		return "", true // no ComparisonError condition
	})
	err := WaitForNamespace(d, "llz-openbao", 30*time.Second)
	if err == nil {
		t.Fatal("want a fail-loud deadline error")
	}
	if strings.Contains(err.Error(), "ComparisonError") {
		t.Errorf("no ComparisonError to report, yet: %q", err)
	}
}
