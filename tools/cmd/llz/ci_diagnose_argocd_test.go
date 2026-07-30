package main

import (
	"os"
	"path/filepath"
	"testing"
)

// effectiveKubeconfig must resolve the config kubectl actually reads: prefer a
// non-empty $KUBECONFIG, otherwise fall back to ~/.kube/config, and only report
// "" (skip diagnostics) when neither is a non-empty file. Gating on $KUBECONFIG
// alone silently skipped the bootstrap-openbao diagnostics — that job uses the
// default path and never exports $KUBECONFIG (the v0.0.19 e2e wedge).
func TestEffectiveKubeconfig(t *testing.T) {
	writeFile := func(t *testing.T, path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("falls back to ~/.kube/config when KUBECONFIG unset", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("KUBECONFIG", "")
		def := filepath.Join(home, ".kube", "config")
		writeFile(t, def, "apiVersion: v1\n")
		if got := effectiveKubeconfig(); got != def {
			t.Fatalf("want default path %q, got %q", def, got)
		}
	})

	t.Run("prefers a non-empty KUBECONFIG", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		kc := filepath.Join(t.TempDir(), "explicit")
		writeFile(t, kc, "apiVersion: v1\n")
		t.Setenv("KUBECONFIG", kc)
		if got := effectiveKubeconfig(); got != kc {
			t.Fatalf("want explicit path %q, got %q", kc, got)
		}
	})

	t.Run("skips when neither exists", func(t *testing.T) {
		home := t.TempDir() // empty: no .kube/config
		t.Setenv("HOME", home)
		t.Setenv("KUBECONFIG", "")
		if got := effectiveKubeconfig(); got != "" {
			t.Fatalf("want empty (nothing to diagnose), got %q", got)
		}
	})

	t.Run("skips when default config is empty", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("KUBECONFIG", "")
		writeFile(t, filepath.Join(home, ".kube", "config"), "") // present but 0 bytes
		if got := effectiveKubeconfig(); got != "" {
			t.Fatalf("want empty for a 0-byte config, got %q", got)
		}
	})
}

// The one-line Application table prints `.status.conditions[*].message`, so an
// app carrying several conditions shows whichever comes first — a benign
// OrphanedResourceWarning masks a SyncError behind it. The full-status dump is
// what makes the real message visible, so its SELECTION must not be the thing
// that loses a failure.
//
// The case that cost an e2e run: an app whose own apply was rejected sits
// OutOfSync while still reporting Healthy (it has no workload to be unhealthy),
// and its parent reports "successfully synced (all tasks run)". Selecting on sync
// AND health independently is what catches it; either alone does not.
func TestNeedsFullAppStatusDump(t *testing.T) {
	for _, tc := range []struct {
		name, appName, sync, health string
		want                        bool
	}{
		{"the run-6 shape: apply rejected, so OutOfSync but nothing unhealthy",
			"llz-openbao", "OutOfSync", "Healthy", true},
		{"degraded workload is equally worth the dump",
			"llz-reconciler", "Synced", "Degraded", true},
		{"status not written yet — a stalled child looks exactly like this",
			"llz-harbor", "", "", true},
		{"fully converged apps are skipped, or the dump buries the failures",
			"keycloak-keycloak", "Synced", "Healthy", false},
		{"platform-bootstrap is skipped — it already gets its own full dump",
			"platform-bootstrap", "OutOfSync", "Healthy", false},
		{"an unparseable/nameless item cannot be fetched",
			"", "OutOfSync", "Degraded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsFullAppStatusDump(tc.appName, tc.sync, tc.health); got != tc.want {
				t.Errorf("needsFullAppStatusDump(%q, %q, %q) = %v, want %v",
					tc.appName, tc.sync, tc.health, got, tc.want)
			}
		})
	}
}
