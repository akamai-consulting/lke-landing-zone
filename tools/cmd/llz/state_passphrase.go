package main

// state_passphrase.go — provisioning and validation for
// TF_STATE_ENCRYPTION_PASSPHRASE, the key OpenTofu encrypts state and plan files
// with (docs/adr/0007-terraform-state-encryption.md).
//
// It is the only credential `llz tokens` MINTS itself rather than prompting for
// or creating through a provider API: there is nothing to create it with. It is
// just entropy, and asking an operator to paste 32 random bytes invites a short
// or hand-typed value — which pbkdf2 would accept and which the HCL-injection
// guard in the terraform-init action would then reject in CI, two steps too late.
//
// ── The property that governs this whole file ────────────────────────────────
//
// Regenerating is CATASTROPHIC, not inconvenient. A second value does not
// "rotate" anything: the state files in the bucket are still encrypted under the
// first one, and OpenTofu's decryption failure is hard (verified — there is no
// fallback to plaintext). So every path here is written to fail closed rather
// than mint a second key:
//
//   - present on GitHub, or cached locally → NEVER regenerate, never even offer.
//   - generated → printed ONCE, with escrow instructions, because this is the
//     only moment the value is recoverable. GitHub will not read it back.
//
// That is also why this is not wired into any rotation lane. Rotating it means
// re-encrypting every state file through OpenTofu's `fallback` key-rollover, not
// setting a new secret — see the ADR.

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// stateEncryptionSecret is the GitHub secret name the terraform-init action
// reads to build TF_ENCRYPTION.
const stateEncryptionSecret = "TF_STATE_ENCRYPTION_PASSPHRASE"

// passphraseBytes is the entropy minted. 32 bytes is the AES-256 key size
// pbkdf2 derives to, and base64 of 32 bytes is 44 chars — comfortably over the
// 16-char pbkdf2 floor.
const passphraseBytes = 32

// passphraseAllowed is the character set the terraform-init action enforces
// before interpolating the value into an HCL string. Kept identical here so a
// bad value is rejected on the operator's machine instead of in a CI log:
// standard base64 (A-Za-z0-9+/=) plus the URL-safe pair, and nothing that could
// close the HCL string and inject encryption configuration.
const passphraseAllowed = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=_-"

// genStateEncryptionPassphrase mints a new passphrase. Package var so tests get
// a deterministic value without stubbing crypto/rand globally.
var genStateEncryptionPassphrase = func() (string, error) {
	b := make([]byte, passphraseBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate %s: %w", stateEncryptionSecret, err)
	}
	// StdEncoding, not URL/Raw: its alphabet is a subset of passphraseAllowed and
	// it is what `openssl rand -base64 32` produces, so a hand-made replacement
	// following the documented command is accepted by the same guard.
	return base64.StdEncoding.EncodeToString(b), nil
}

// validateStateEncryptionPassphrase mirrors the terraform-init preflight. The
// returned error is phrased for someone holding the value, not reading a job log.
func validateStateEncryptionPassphrase(v string) error {
	if v == "" {
		return fmt.Errorf("%s is empty", stateEncryptionSecret)
	}
	if len(v) < 16 {
		return fmt.Errorf("%s is %d characters; pbkdf2 requires at least 16 — regenerate with `openssl rand -base64 32`", stateEncryptionSecret, len(v))
	}
	if i := strings.IndexFunc(v, func(r rune) bool { return !strings.ContainsRune(passphraseAllowed, r) }); i >= 0 {
		// Deliberately reports the POSITION, never the character: echoing it would
		// leak a byte of the key into a terminal and any scrollback behind it.
		return fmt.Errorf("%s contains a character at position %d outside [A-Za-z0-9+/=_-]. "+
			"The value is interpolated into an HCL string, where a quote or backslash could close it and inject "+
			"encryption configuration — so this is refused rather than escaped. Regenerate with `openssl rand -base64 32`",
			stateEncryptionSecret, i+1)
	}
	return nil
}

// ensureStateEncryptionPassphrase provisions the passphrase into `secrets` when
// it is not already configured, and returns whether it minted one (so the caller
// can print the escrow banner exactly once, at the only moment the value exists
// outside GitHub).
//
// `configured` is the caller's have()-style predicate: true when the secret is
// already set on the instance repo or cached in .llz/secrets.env. When it is
// true this function does NOTHING — see the file header.
func ensureStateEncryptionPassphrase(secrets map[string]string, configured bool) (minted bool, err error) {
	if configured {
		return false, nil
	}
	if v := secrets[stateEncryptionSecret]; v != "" {
		// Already gathered this run (or restored from cache) — validate, don't mint.
		return false, validateStateEncryptionPassphrase(v)
	}
	v, err := genStateEncryptionPassphrase()
	if err != nil {
		return false, err
	}
	if err := validateStateEncryptionPassphrase(v); err != nil {
		return false, fmt.Errorf("generated an invalid passphrase (this is a bug): %w", err)
	}
	secrets[stateEncryptionSecret] = v
	return true, nil
}

// stateEncryptionEscrowNotice is the one-time banner. Split out so its wording is
// asserted directly rather than through captured stdout.
//
// It prints the VALUE because this is the only moment it is recoverable: GitHub
// stores secrets write-only, and `llz` caches to .llz/secrets.env which is
// gitignored and local. An operator who closes this terminal without copying it
// has lost nothing yet — the state files are not encrypted until the next apply —
// but they must re-run before that happens, so the notice says so.
func stateEncryptionEscrowNotice(value string) []string {
	return []string{
		"",
		bold("⚠  " + stateEncryptionSecret + " was generated — ESCROW IT NOW"),
		"",
		"    " + value,
		"",
		"    This key encrypts every Terraform state and plan file (ADR 0007).",
		"    LOSING IT MAKES EVERY STATE FILE UNRECOVERABLE — there is no recovery",
		"    path and no fallback to plaintext. Escrow it offline with the same",
		"    discipline as OPENBAO_SEAL_KEY, in a different place from the state",
		"    bucket credentials (a copy stored beside TF_STATE_ACCESS_KEY defeats",
		"    the point: whoever holds both can read the state).",
		"",
		"    It is NOT shown again. GitHub stores secrets write-only, and a second",
		"    run will not mint another one — a replacement key cannot decrypt state",
		"    written under this one.",
		"",
	}
}
