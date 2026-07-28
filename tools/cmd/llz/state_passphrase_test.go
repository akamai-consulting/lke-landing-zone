package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// THE test. A second passphrase does not rotate anything — the state files in
// the bucket are still encrypted under the first one, and OpenTofu's decryption
// failure is hard. So an already-configured secret must never be touched, by any
// path.
func TestEnsureStateEncryptionPassphraseNeverRegenerates(t *testing.T) {
	prev := genStateEncryptionPassphrase
	t.Cleanup(func() { genStateEncryptionPassphrase = prev })
	called := 0
	genStateEncryptionPassphrase = func() (string, error) { called++; return "SHOULD-NEVER-BE-MINTED", nil }

	// Configured on GitHub / in the local cache.
	secrets := map[string]string{}
	minted, err := ensureStateEncryptionPassphrase(secrets, true)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if minted || called != 0 {
		t.Errorf("a configured passphrase must not be regenerated (minted=%v, generator called %d times)", minted, called)
	}
	if _, ok := secrets[stateEncryptionSecret]; ok {
		t.Error("must not write a value when one is already configured")
	}
}

// Gathered earlier in the same run (or restored from .llz cache): validate it,
// do not mint a second one.
// NOTE on the fixtures below: they are deliberately readable rather than
// realistic base64. A base64-shaped literal in a file named *passphrase* trips
// gitleaks' generic-api-key rule, and the honest fix is a fixture that does not
// look like a credential — not an allowlist entry, which would also silence a
// REAL key accidentally committed here later.
func TestEnsureStateEncryptionPassphraseKeepsAnExistingValue(t *testing.T) {
	prev := genStateEncryptionPassphrase
	t.Cleanup(func() { genStateEncryptionPassphrase = prev })
	genStateEncryptionPassphrase = func() (string, error) { t.Fatal("must not mint"); return "", nil }

	secrets := map[string]string{stateEncryptionSecret: "not-a-real-passphrase-fixture-only"}
	minted, err := ensureStateEncryptionPassphrase(secrets, false)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if minted {
		t.Error("an already-present value must not count as minted — the escrow banner would claim a new key")
	}
}

// A cached value that would be rejected by the terraform-init preflight must
// fail HERE, not in a CI container.
func TestEnsureStateEncryptionPassphraseRejectsABadCachedValue(t *testing.T) {
	secrets := map[string]string{stateEncryptionSecret: `oops"quote`}
	if _, err := ensureStateEncryptionPassphrase(secrets, false); err == nil {
		t.Fatal("a value outside the allowed charset must be rejected")
	}
}

func TestEnsureStateEncryptionPassphraseMints(t *testing.T) {
	secrets := map[string]string{}
	minted, err := ensureStateEncryptionPassphrase(secrets, false)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !minted {
		t.Fatal("an unconfigured passphrase must be minted")
	}
	v := secrets[stateEncryptionSecret]
	if err := validateStateEncryptionPassphrase(v); err != nil {
		t.Errorf("minted value fails its own validator: %v", err)
	}
	// 32 random bytes -> 44 base64 chars. Well clear of the pbkdf2 16-char floor.
	if len(v) < 40 {
		t.Errorf("minted value is only %d chars", len(v))
	}
}

// Two mints must differ — a deterministic generator would hand every instance in
// the fleet the same state-encryption key.
func TestGenStateEncryptionPassphraseIsRandom(t *testing.T) {
	a, err := genStateEncryptionPassphrase()
	if err != nil {
		t.Fatal(err)
	}
	b, err := genStateEncryptionPassphrase()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two mints produced the same passphrase")
	}
}

func TestValidateStateEncryptionPassphrase(t *testing.T) {
	cases := []struct {
		name, in, wantErr string
	}{
		{"empty", "", "is empty"},
		{"too short", "abc123", "at least 16"},
		{"double quote closes the HCL string", `abcdefghijklmnop"x`, "position"},
		{"backslash escapes out", `abcdefghijklmnop\x`, "position"},
		{"newline injects a line", "abcdefghijklmnop\nmethod", "position"},
		{"space", "abcdefghijklmnop x", "position"},
		{"standard base64 output", "not-a-real-passphrase-fixture-only", ""},
		{"url-safe alphabet", "abcdefghijklmnop_-", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateStateEncryptionPassphrase(c.in)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
			// The offending character must never be echoed — it is a byte of the
			// key, and a terminal keeps scrollback.
			if c.in != "" && err != nil && strings.Contains(err.Error(), c.in) {
				t.Errorf("error must not echo the value: %v", err)
			}
		})
	}
}

func TestEnsureStateEncryptionPassphraseSurfacesAGeneratorFailure(t *testing.T) {
	prev := genStateEncryptionPassphrase
	t.Cleanup(func() { genStateEncryptionPassphrase = prev })
	genStateEncryptionPassphrase = func() (string, error) { return "", errors.New("no entropy") }
	if _, err := ensureStateEncryptionPassphrase(map[string]string{}, false); err == nil {
		t.Error("a generator failure must not silently leave the secret unset")
	}
}

// The banner is the ONLY moment the value is recoverable, so it has to carry
// both the value and the consequence of losing it.
func TestStateEncryptionEscrowNoticeCarriesValueAndStakes(t *testing.T) {
	out := strings.Join(stateEncryptionEscrowNotice("SECRET-VALUE-HERE"), "\n")
	for _, want := range []string{"SECRET-VALUE-HERE", "ESCROW IT NOW", "UNRECOVERABLE", "NOT shown again"} {
		if !strings.Contains(out, want) {
			t.Errorf("escrow notice missing %q:\n%s", want, out)
		}
	}
}

// Scope is declared in the requirements table, not inferred from the name. The
// passphrase MUST be repo-level: the vpc root's state is shared across the
// deployments attached to one network, so a per-env key would let one env
// encrypt a state file its peer cannot decrypt.
func TestSecretScoping(t *testing.T) {
	if secretIsEnvScoped(stateEncryptionSecret) {
		t.Error("TF_STATE_ENCRYPTION_PASSPHRASE must be repo-level — the shared vpc state is encrypted once for all deployments")
	}
	for _, envScoped := range []string{"LINODE_API_TOKEN", "TF_STATE_ACCESS_KEY", "OPENBAO_SECRETS_WRITE_TOKEN"} {
		if !secretIsEnvScoped(envScoped) {
			t.Errorf("%s should stay infra-<env>-scoped", envScoped)
		}
	}
	// Unknown names default to env-scoped (the historical behaviour).
	if !secretIsEnvScoped("SOME_FUTURE_SECRET") {
		t.Error("unknown secrets should default to env-scoped")
	}
}

// doctor/tokens report it as well-formed-but-unverifiable, never as "valid" in
// the sense the other rows mean — nothing can prove it is the RIGHT key until a
// decrypt is attempted.
func TestPassphraseProbeReportsShapeOnly(t *testing.T) {
	tv := probeToken(stateEncryptionSecret, "not-a-real-passphrase-fixture-only", "", time.Unix(0, 0))
	if tv.status != vValid {
		t.Fatalf("a well-formed passphrase should pass shape validation, got %v (%s)", tv.status, tv.detail)
	}
	if !strings.Contains(tv.detail, "NOT verifiable") {
		t.Errorf("detail must not imply the key was verified against anything: %q", tv.detail)
	}
	bad := probeToken(stateEncryptionSecret, "short", "", time.Unix(0, 0))
	if bad.status != vInvalid {
		t.Errorf("a too-short passphrase must be reported invalid, got %v", bad.status)
	}
}
