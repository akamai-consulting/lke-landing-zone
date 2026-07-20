package main

import (
	"strings"
	"testing"
	"time"
)

// assertArgoAppDeps builds seam deps: kubectl answers from the script (keyed
// by joined args prefix), a fake clock advanced by sleep.
func assertArgoAppDeps(t *testing.T, script func(call int, args []string) (string, bool)) (aplGateDeps, *int) {
	t.Helper()
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

// Healthy path: the app appears on the second existence probe.
func TestAssertArgoAppAppears(t *testing.T) {
	d, _ := assertArgoAppDeps(t, func(call int, args []string) (string, bool) {
		if strings.Contains(strings.Join(args, " "), "get application.argoproj.io platform-openbao") {
			return "", call > 2 // missing on the first probe, present later
		}
		return "Running\tsyncing wave -15", true // parent state probe
	})
	if err := assertArgoApp(d, "argocd", "platform-openbao", "platform-bootstrap", 4*time.Minute); err != nil {
		t.Fatalf("app appeared but gate failed: %v", err)
	}
}

// A terminally Failed parent short-circuits immediately with its message.
func TestAssertArgoAppTerminalParentFailsFast(t *testing.T) {
	d, calls := assertArgoAppDeps(t, func(_ int, args []string) (string, bool) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "platform-openbao") {
			return "", false
		}
		return "Failed\tone or more synchronization tasks completed unsuccessfully", true
	})
	err := assertArgoApp(d, "argocd", "platform-openbao", "platform-bootstrap", 4*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "terminally Failed") {
		t.Fatalf("want terminal-failure error, got %v", err)
	}
	// One cycle = existence probe + operationState probe + the failure-diag probe.
	// More than that means it looped instead of short-circuiting on the terminal phase.
	if *calls > 3 {
		t.Fatalf("terminal parent must short-circuit on the first cycle, made %d kubectl calls", *calls)
	}
}

// argoParentDiag surfaces the parent's sync/health/conditions (where a child
// ComparisonError shows) — the signal operationState misses when no sync ran.
func TestArgoParentDiag(t *testing.T) {
	d, _ := assertArgoAppDeps(t, func(_ int, _ []string) (string, bool) {
		return "OutOfSync/Missing [ComparisonError: app path does not exist]", true
	})
	got := argoParentDiag(d, "argocd", "platform-bootstrap")
	if !strings.Contains(got, "ComparisonError") || !strings.Contains(got, "platform-bootstrap") {
		t.Fatalf("diag should carry the parent name + condition, got %q", got)
	}

	// Unreadable parent (kubectl fails) yields a hint, not an empty string.
	d2, _ := assertArgoAppDeps(t, func(_ int, _ []string) (string, bool) { return "", false })
	if got := argoParentDiag(d2, "argocd", "platform-bootstrap"); !strings.Contains(got, "unavailable") {
		t.Fatalf("unreadable parent should hint 'unavailable', got %q", got)
	}
}

// A transient git-fetch ComparisonError on the parent app-of-apps must be recovered
// by a forced hard refresh — the app then appears and the gate passes.
func TestAssertArgoAppRecoversFromTransientComparisonError(t *testing.T) {
	refreshed := false
	d, _ := assertArgoAppDeps(t, func(_ int, args []string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "annotate") && strings.Contains(joined, "refresh=hard"):
			refreshed = true
			return "", true
		case strings.Contains(joined, "get application.argoproj.io platform-openbao"):
			return "", refreshed // appears only after the forced re-fetch
		case strings.Contains(joined, "ComparisonError"):
			return "failed to generate manifest: rpc error: failed to list refs: repository not found", true
		default:
			return "\t", true // operationState probe: no sync operation started
		}
	})
	if err := assertArgoApp(d, "argocd", "platform-openbao", "platform-bootstrap", 2*time.Minute); err != nil {
		t.Fatalf("transient ComparisonError should recover via hard refresh, got %v", err)
	}
	if !refreshed {
		t.Fatal("expected a hard refresh to be forced on the transient fetch ComparisonError")
	}
}

// A NON-transient ComparisonError (a real manifest error) must NOT be refresh-recovered
// — recovery must never mask a genuine break; the gate fails at the deadline.
func TestAssertArgoAppNonTransientComparisonErrorNotRefreshed(t *testing.T) {
	refreshed := false
	d, _ := assertArgoAppDeps(t, func(_ int, args []string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "annotate") && strings.Contains(joined, "refresh=hard"):
			refreshed = true
			return "", true
		case strings.Contains(joined, "get application.argoproj.io platform-openbao"):
			return "", false // never appears
		case strings.Contains(joined, "ComparisonError"):
			return "failed to generate manifest: map[apiVersion:v1] is missing required field kind", true
		default:
			return "\t", true
		}
	})
	err := assertArgoApp(d, "argocd", "platform-openbao", "platform-bootstrap", 40*time.Second)
	if err == nil || !strings.Contains(err.Error(), "not created within") {
		t.Fatalf("want deadline error for a real ComparisonError, got %v", err)
	}
	if refreshed {
		t.Fatal("a non-transient ComparisonError must NOT trigger a hard refresh")
	}
}

// A git-auth ComparisonError that OUTLIVES the grace is terminal: the gate must
// abort well before the deadline and must never have nudged (a hard refresh
// re-asks a question the remote already answered).
func TestAssertArgoAppGitAuthIsTerminalAfterGrace(t *testing.T) {
	refreshed := false
	d, _ := assertArgoAppDeps(t, func(_ int, args []string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "annotate") && strings.Contains(joined, "refresh=hard"):
			refreshed = true
			return "", true
		case strings.Contains(joined, "get application.argoproj.io platform-openbao"):
			return "", false // never appears
		case strings.Contains(joined, "ComparisonError"):
			return "failed to generate manifest for source 1 of 1: rpc error: code = Unknown desc = failed to list refs: authentication required: Unauthorized", true
		default:
			return "\t", true
		}
	})
	// Deadline far beyond the grace, so a "not created within" verdict would prove
	// it burned the window instead of aborting.
	err := assertArgoApp(d, "argocd", "platform-openbao", "platform-bootstrap", 30*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "cannot authenticate to the source repo") {
		t.Fatalf("want terminal git-auth error, got %v", err)
	}
	if refreshed {
		t.Fatal("a git-auth refusal must NOT trigger a hard refresh — the remote already answered")
	}
}

// ...but a git-auth error that clears inside the grace must NOT abort: the argocd
// repo Secret arrives via an ExternalSecret, and an Application reconciling in
// that gap can report an auth failure it recovers from on its own.
func TestAssertArgoAppGitAuthWithinGraceRecovers(t *testing.T) {
	cycle := 0
	d, _ := assertArgoAppDeps(t, func(_ int, args []string) (string, bool) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "get application.argoproj.io platform-openbao"):
			cycle++
			// Appears once the repo Secret lands — inside gitAuthGrace (2m @ 10s/cycle).
			return "", cycle > 4
		case strings.Contains(joined, "ComparisonError"):
			return "failed to list refs: authentication required: Unauthorized", true
		default:
			return "\t", true
		}
	})
	if err := assertArgoApp(d, "argocd", "platform-openbao", "platform-bootstrap", 30*time.Minute); err != nil {
		t.Fatalf("a git-auth error that clears inside the grace must not abort: %v", err)
	}
}

func TestTransientFetchError(t *testing.T) {
	for _, m := range []string{
		"failed to list refs: repository not found",
		"hit 27s timeout running git fetch",
		"dial tcp: i/o timeout",
		"rpc error: code = Unknown",
	} {
		if !transientFetchError(m) {
			t.Errorf("should be transient: %q", m)
		}
	}
	for _, m := range []string{
		"", "is missing required field kind", "invalid yaml at line 3",
		"kind ExternalSecret not registered",
	} {
		if transientFetchError(m) {
			t.Errorf("should NOT be transient: %q", m)
		}
	}
}

// A Running-but-retrying sync (by-design first-boot transients) must NOT fail
// fast — it fails only at the deadline, carrying the parent's message.
func TestAssertArgoAppRetryingParentWaitsForDeadline(t *testing.T) {
	d, _ := assertArgoAppDeps(t, func(_ int, args []string) (string, bool) {
		if strings.Contains(strings.Join(args, " "), "platform-openbao") {
			return "", false
		}
		return "Running\tcompleted unsuccessfully … Retrying attempt #3", true
	})
	err := assertArgoApp(d, "argocd", "platform-openbao", "platform-bootstrap", 90*time.Second)
	if err == nil || !strings.Contains(err.Error(), "not created within") {
		t.Fatalf("want deadline error for retrying parent, got %v", err)
	}
}
