package main

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withKubectlApply (ci_openbao_ca_test.go) records the last applied manifest;
// withSeedRand (ci_ensure_secret_test.go) makes the generated key deterministic.

// withSeedNamespace makes waitForOpenbaoNamespace resolve immediately (namespace
// present) with no real sleeping, so the Secret-logic tests below exercise the
// key handling without the convergence wait. The wait itself is covered by
// TestWaitForOpenbaoNamespace*.
func withSeedNamespace(t *testing.T, present bool) {
	t.Helper()
	origExists, origSleep := seedNamespaceExists, seedSleep
	seedNamespaceExists = func(string) bool { return present }
	seedSleep = func(time.Duration) {}
	t.Cleanup(func() { seedNamespaceExists, seedSleep = origExists, origSleep })
}

// namespace appears after a few polls → success, no error.
func TestWaitForOpenbaoNamespaceAppears(t *testing.T) {
	origExists, origSleep := seedNamespaceExists, seedSleep
	t.Cleanup(func() { seedNamespaceExists, seedSleep = origExists, origSleep })
	calls := 0
	seedNamespaceExists = func(string) bool { calls++; return calls >= 3 }
	slept := 0
	seedSleep = func(time.Duration) { slept++ }
	if err := waitForOpenbaoNamespace("llz-openbao", openbaoNSWait); err != nil {
		t.Fatalf("should succeed once the namespace appears: %v", err)
	}
	if calls != 3 || slept != 2 {
		t.Errorf("calls=%d slept=%d, want 3 probes / 2 sleeps", calls, slept)
	}
}

// namespace never appears → fail loud at the deadline (no infinite spin).
func TestWaitForOpenbaoNamespaceTimesOut(t *testing.T) {
	origExists, origSleep := seedNamespaceExists, seedSleep
	t.Cleanup(func() { seedNamespaceExists, seedSleep = origExists, origSleep })
	seedNamespaceExists = func(string) bool { return false }
	seedSleep = func(time.Duration) {}
	err := waitForOpenbaoNamespace("llz-openbao", 20*time.Second)
	if err == nil || !strings.Contains(err.Error(), "not found after") {
		t.Errorf("err = %v, want a fail-loud timeout", err)
	}
}

func TestSealKeySecretManifest(t *testing.T) {
	key := make([]byte, sealKeyBytes)
	for i := range key {
		key[i] = 0xAB
	}
	m := sealKeySecretManifest("llz-openbao", "openbao-unseal-key", key)
	wantB64 := base64.StdEncoding.EncodeToString(key)
	for _, want := range []string{
		"kind: Secret",
		"name: openbao-unseal-key",
		"namespace: llz-openbao",
		"type: Opaque",
		"unseal.key: " + wantB64,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q:\n%s", want, m)
		}
	}
}

// existing Secret → idempotent no-op: nothing applied, no key generated.
func TestRunCIBaoSeedSealKeyExistingIsNoop(t *testing.T) {
	withSeedNamespace(t, true)
	t.Setenv("OPENBAO_SEAL_KEY", "")
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte("openbao-unseal-key"), nil }) // get secret succeeds
	applied := withKubectlApply(t)
	gh := withGHSetSecret(t, nil)
	if err := runCIBaoSeedSealKey(globalOpts{}, "primary"); err != nil {
		t.Fatal(err)
	}
	if *applied != "" || len(*gh) != 0 {
		t.Errorf("existing secret must not apply (%q) or persist (%d)", *applied, len(*gh))
	}
}

// absent Secret + OPENBAO_SEAL_KEY present → restore that key, no gh write.
func TestRunCIBaoSeedSealKeyRestoreFromEnv(t *testing.T) {
	withSeedNamespace(t, true)
	key := make([]byte, sealKeyBytes)
	for i := range key {
		key[i] = 0x7
	}
	enc := base64.StdEncoding.EncodeToString(key)
	t.Setenv("OPENBAO_SEAL_KEY", enc)
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("NotFound") }) // get secret fails → absent
	applied := withKubectlApply(t)
	gh := withGHSetSecret(t, nil)
	if err := runCIBaoSeedSealKey(globalOpts{}, "primary"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*applied, "unseal.key: "+enc) {
		t.Errorf("restore must apply the env key, got %q", *applied)
	}
	if len(*gh) != 0 {
		t.Errorf("restore must not re-persist to gh: %v", *gh)
	}
}

// reject a malformed restore value rather than seed a wrong-length key.
func TestRunCIBaoSeedSealKeyRestoreBadLength(t *testing.T) {
	withSeedNamespace(t, true)
	t.Setenv("OPENBAO_SEAL_KEY", base64.StdEncoding.EncodeToString([]byte("too-short")))
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("NotFound") })
	withKubectlApply(t)
	if err := runCIBaoSeedSealKey(globalOpts{}, "primary"); err == nil || !strings.Contains(err.Error(), "want 32") {
		t.Errorf("err = %v, want wrong-length rejection", err)
	}
}

// absent Secret, nothing to restore, GH_TOKEN present → generate, persist for
// DR, apply, and write the offline-backup banner.
func TestRunCIBaoSeedSealKeyGenerate(t *testing.T) {
	withSeedNamespace(t, true)
	t.Setenv("OPENBAO_SEAL_KEY", "")
	t.Setenv("GH_TOKEN", "ghp_write")
	sum := filepath.Join(t.TempDir(), "summary")
	t.Setenv("GITHUB_STEP_SUMMARY", sum)
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("NotFound") })
	withSeedRand(t, 0x42)
	applied := withKubectlApply(t)
	gh := withGHSetSecret(t, nil)

	if err := runCIBaoSeedSealKey(globalOpts{}, "primary"); err != nil {
		t.Fatal(err)
	}

	key := make([]byte, sealKeyBytes)
	for i := range key {
		key[i] = 0x42
	}
	enc := base64.StdEncoding.EncodeToString(key)
	if !strings.Contains(*applied, "unseal.key: "+enc) {
		t.Errorf("generate must apply the new key, got %q", *applied)
	}
	if strings.Join(*gh, " ") != "OPENBAO_SEAL_KEY@infra-primary="+enc {
		t.Errorf("gh persistence = %v, want OPENBAO_SEAL_KEY@infra-primary", *gh)
	}
	if b, _ := os.ReadFile(sum); !strings.Contains(string(b), "Back It Up Now") {
		t.Errorf("summary missing offline-backup banner: %q", b)
	}
}

// generate path with no secrets-write PAT is fatal — the DR copy can't be saved.
func TestRunCIBaoSeedSealKeyGenerateNeedsGHToken(t *testing.T) {
	withSeedNamespace(t, true)
	t.Setenv("OPENBAO_SEAL_KEY", "")
	t.Setenv("GH_TOKEN", "")
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("NotFound") })
	applied := withKubectlApply(t)
	if err := runCIBaoSeedSealKey(globalOpts{}, "primary"); err == nil || !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Errorf("err = %v, want GH_TOKEN-required error", err)
	}
	if *applied != "" {
		t.Error("must not apply a key it cannot back up")
	}
}

func TestRunCIBaoSeedSealKeyDryRunAndWiring(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		t.Error("dry-run must not exec kubectl")
		return nil, nil
	})
	if err := runCIBaoSeedSealKey(globalOpts{dryRun: true}, "primary"); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if err := runCIBaoSeedSealKey(globalOpts{}, ""); err == nil || !strings.Contains(err.Error(), "--region") {
		t.Errorf("missing region = %v, want --region error", err)
	}
	if c := ciBaoSeedSealKeyCmd(); c.Use != "bao-seed-seal-key" {
		t.Errorf("Use = %q, want bao-seed-seal-key", c.Use)
	}
}
