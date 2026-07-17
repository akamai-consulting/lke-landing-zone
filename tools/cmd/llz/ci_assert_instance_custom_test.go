package main

import (
	"strings"
	"testing"
	"time"
)

// instanceCustomDeps builds seam deps whose kubectl answers from a per-call script
// and whose clock advances by sleep, so deadline loops terminate without real waits.
func instanceCustomDeps(script func(call int, args []string) (string, bool)) (aplGateDeps, *int) {
	now := time.Unix(0, 0)
	calls := 0
	return aplGateDeps{
		kubectl: func(args ...string) (string, bool) {
			calls++
			return script(calls, args)
		},
		now:   func() time.Time { return now },
		sleep: func(d time.Duration) { now = now.Add(d) },
	}, &calls
}

// Happy path: the App appears on a later existence probe, then reads Synced+Healthy.
func TestAssertInstanceCustomHappyPath(t *testing.T) {
	d, _ := instanceCustomDeps(func(call int, args []string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "jsonpath={.status.sync.status}"):
			return "Synced\tHealthy", true
		case strings.Contains(joined, "get application.argoproj.io instance-custom-llz-e2e-custom"):
			return "", call > 2 // missing on the first existence probe, present later
		default:
			return "", true
		}
	})
	if err := assertInstanceCustom(d, "llz-e2e-custom", "instance-custom", 5*time.Minute); err != nil {
		t.Fatalf("hatch synced but gate failed: %v", err)
	}
}

// A generated App that never appears fails at the deadline WITH the ApplicationSet
// diagnostics, and never advances to the sync/health probe.
func TestAssertInstanceCustomNeverGeneratedFailsWithAppSetDiag(t *testing.T) {
	d, _ := instanceCustomDeps(func(_ int, args []string) (string, bool) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "get applicationset instance-custom") {
			return "[ErrorOccurred: True unable to generate: repository not found]", true
		}
		// The generated Application never exists.
		return "", false
	})
	err := assertInstanceCustom(d, "llz-e2e-custom", "instance-custom", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "not generated") {
		t.Fatalf("want a not-generated failure, got %v", err)
	}
	// It must never probe sync/health for an app that never appeared.
	if d.kubectl == nil { // guard against accidental nil in refactors
		t.Fatal("kubectl seam went nil")
	}
}

// The App exists but never reaches Synced+Healthy (stuck OutOfSync) → deadline fail.
func TestAssertInstanceCustomStuckOutOfSyncFails(t *testing.T) {
	d, _ := instanceCustomDeps(func(_ int, args []string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "jsonpath={.status.sync.status}"):
			return "OutOfSync\tMissing", true // never converges
		case strings.Contains(joined, "get application.argoproj.io"):
			return "", true // exists
		default:
			return "", true
		}
	})
	err := assertInstanceCustom(d, "llz-e2e-custom", "instance-custom", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "not Synced+Healthy") {
		t.Fatalf("want a not-Synced+Healthy failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "sync=OutOfSync") {
		t.Fatalf("failure should carry the last-observed sync/health, got %v", err)
	}
}

// Health-inert ConfigMap race: Healthy but still OutOfSync must NOT pass — requiring
// Synced is what proves the seeded manifest was actually applied.
func TestAssertInstanceCustomHealthyButOutOfSyncDoesNotPass(t *testing.T) {
	d, _ := instanceCustomDeps(func(_ int, args []string) (string, bool) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "jsonpath={.status.sync.status}") {
			return "OutOfSync\tHealthy", true
		}
		return "", true // app exists
	})
	if err := assertInstanceCustom(d, "llz-e2e-custom", "instance-custom", 30*time.Second); err == nil {
		t.Fatal("Healthy-but-OutOfSync must not satisfy the gate")
	}
}

// The App name is derived from the namespace flag (instance-custom-<ns>).
func TestAssertInstanceCustomDerivesAppName(t *testing.T) {
	var probedApp string
	d, _ := instanceCustomDeps(func(_ int, args []string) (string, bool) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "get application.argoproj.io instance-custom-") {
			probedApp = "seen"
		}
		if strings.Contains(joined, "jsonpath={.status.sync.status}") {
			return "Synced\tHealthy", true
		}
		return "", true
	})
	if err := assertInstanceCustom(d, "team-a", "instance-custom", time.Minute); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if probedApp == "" {
		t.Fatal("expected an existence probe for instance-custom-<ns>")
	}
}

// appSetDiag carries the ApplicationSet name + condition text; an unreadable
// ApplicationSet yields a hint, not an empty string.
func TestAppSetDiag(t *testing.T) {
	d, _ := instanceCustomDeps(func(_ int, _ []string) (string, bool) {
		return "[ErrorOccurred: True bad directory name]", true
	})
	if got := appSetDiag(d, "instance-custom"); !strings.Contains(got, "ErrorOccurred") || !strings.Contains(got, "instance-custom") {
		t.Fatalf("diag should carry the appset name + condition, got %q", got)
	}
	d2, _ := instanceCustomDeps(func(_ int, _ []string) (string, bool) { return "", false })
	if got := appSetDiag(d2, "instance-custom"); !strings.Contains(got, "unavailable") {
		t.Fatalf("unreadable appset should yield a hint, got %q", got)
	}
}
