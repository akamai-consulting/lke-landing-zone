package main

import (
	"strings"
	"testing"
)

// The kubectl seam folds stderr into stdout, so a client-side-throttling warning
// (or any other kubectl noise) can precede the JSON body. argoSyncHealth must
// skip to the '{' rather than hand the whole blob to Unmarshal.
func TestArgoSyncHealthSkipsLeadingNoise(t *testing.T) {
	d, _ := instanceCustomDeps(func(_ int, _ []string) (string, bool) {
		return "I0101 12:00:00.000000 1 request.go:697] Waited for 1.1s due to client-side throttling\n" +
			appStatusJSON("Synced", "Healthy"), true
	})
	sync, health := argoSyncHealth(d, "argocd", "platform-openbao")
	if sync != "Synced" || health != "Healthy" {
		t.Errorf("argoSyncHealth = (%q,%q), want (Synced,Healthy) — leading kubectl noise must be skipped", sync, health)
	}
}

// No '{' anywhere: blanks, never a slice-bounds panic (IndexByte returns -1).
func TestArgoSyncHealthNoJSONBodyIsBlank(t *testing.T) {
	d, _ := instanceCustomDeps(func(_ int, _ []string) (string, bool) {
		return "Error from server (NotFound): applications.argoproj.io \"x\" not found\n", true
	})
	if sync, health := argoSyncHealth(d, "argocd", "x"); sync != "" || health != "" {
		t.Errorf("argoSyncHealth = (%q,%q), want blanks for a bodyless response", sync, health)
	}
}

// argoAppDiag reports the jsonpath summary whenever the read SUCCEEDED and
// returned something; "state unavailable" is only for an unreadable/empty read.
// The readable direction had no coverage, so an `!ok || out == ""` slip that
// swallowed every real summary was invisible.
func TestArgoAppDiag(t *testing.T) {
	readable, _ := instanceCustomDeps(func(_ int, _ []string) (string, bool) {
		return "  OutOfSync/Degraded [ComparisonError: bad kind] op=failed\n", true
	})
	got := argoAppDiag(readable, "argocd", "instance-custom-e2e")
	if !strings.Contains(got, "instance-custom-e2e sync/health/conditions:") {
		t.Errorf("diag = %q, want the sync/health/conditions summary", got)
	}
	if !strings.Contains(got, "OutOfSync/Degraded") || strings.Contains(got, "state unavailable") {
		t.Errorf("diag = %q, want the trimmed jsonpath payload", got)
	}

	unreadable, _ := instanceCustomDeps(func(_ int, _ []string) (string, bool) { return "", false })
	if got := argoAppDiag(unreadable, "argocd", "gone"); !strings.Contains(got, "state unavailable") {
		t.Errorf("diag = %q, want the unavailable fallback when the read fails", got)
	}
	blank, _ := instanceCustomDeps(func(_ int, _ []string) (string, bool) { return "   \n", true })
	if got := argoAppDiag(blank, "argocd", "empty"); !strings.Contains(got, "state unavailable") {
		t.Errorf("diag = %q, want the unavailable fallback for an all-whitespace read", got)
	}
}
