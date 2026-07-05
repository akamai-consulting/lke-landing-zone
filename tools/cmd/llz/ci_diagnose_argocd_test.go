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
