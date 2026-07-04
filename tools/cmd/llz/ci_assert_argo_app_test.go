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
	if *calls > 2 {
		t.Fatalf("terminal parent must short-circuit on the first cycle, made %d kubectl calls", *calls)
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
