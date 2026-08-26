// Package tfenc builds the TF_ENCRYPTION document OpenTofu merges with each root's
// `encryption` block, and assembles the rest of the environment a hand-run `tofu`
// needs inside an instance checkout.
//
// THE CANONICAL STATEMENT OF WHY, which the rest of this feature points at rather
// than repeating: `encryption.tf` carries only the enforcement posture, so the key
// material arrives from $TF_ENCRYPTION and an apply without it FAILS instead of
// silently writing plaintext state (ADR 0007 (state encryption)). That tripwire
// also fires on the operator — a hand-run `tofu` dies on OpenTofu's own "Invalid
// expression … A single static variable reference is required", which names
// neither the passphrase nor the variable. The material to build the document has
// been in `.llz/secrets.env` since `llz tokens` ran; Hydrate connects the two.
//
// The document has two emitters — the `tf-encryption-env` composite action for
// CI, and this package for everything else — because CI cannot depend on the llz
// binary being present. tfenc_test.go pins them together by RUNNING the action's
// shell body and diffing it, rather than restating the format in a fixture.
package tfenc

import (
	"fmt"
	"strings"
)

// The environment variables this package reads and writes. Names, not literals
// scattered across call sites: the composite action and the `.llz` cache use the
// same spellings, and a typo in one of them is a silent fallthrough to "not
// configured" rather than an error.
const (
	// EnvVar is what OpenTofu itself reads.
	EnvVar = "TF_ENCRYPTION"

	// PassphraseEnv / KeyNameEnv are the CURRENT key's coordinates.
	PassphraseEnv = "TF_STATE_ENCRYPTION_PASSPHRASE"
	KeyNameEnv    = "TF_STATE_ENCRYPTION_KEY_NAME"

	// PassphraseOldEnv / KeyNameOldEnv are set only during a rotation window.
	PassphraseOldEnv = "TF_STATE_ENCRYPTION_PASSPHRASE_OLD"
	KeyNameOldEnv    = "TF_STATE_ENCRYPTION_KEY_NAME_OLD"

	// DefaultKeyName is the name every state file written before rotation
	// existed was encrypted under. LOAD-BEARING: OpenTofu stores the pbkdf2 SALT
	// under meta["key_provider.pbkdf2.<name>"], so a passphrase can only decrypt
	// state written under the SAME name. This default is not cosmetic and must
	// match the composite action's `state-encryption-key-name` default.
	DefaultKeyName = "llz"

	// MinPassphraseLen is pbkdf2's floor. OpenTofu rejects anything shorter at
	// init, with a message about the key provider rather than about the secret —
	// so both emitters check it here and say which secret is wrong.
	MinPassphraseLen = 16
)

// Config is the key material a TF_ENCRYPTION document is rendered from.
//
// The zero value is not usable: Build validates rather than defaults, except for
// KeyName, whose default is a documented compatibility constant rather than a
// convenience.
type Config struct {
	// Passphrase is TF_STATE_ENCRYPTION_PASSPHRASE — the current key.
	Passphrase string
	// KeyName is the HCL name of the current pbkdf2 key provider. Empty means
	// DefaultKeyName.
	KeyName string

	// PassphraseOld and KeyNameOld describe the PREVIOUS key, and are set only
	// during a rotation window. When PassphraseOld is set, Build emits a second
	// key provider and an ENCRYPTED `state` fallback so OpenTofu reads state
	// written with the old key and rewrites it with the new one. KeyNameOld is
	// required whenever PassphraseOld is set — the fallback cannot locate its
	// pbkdf2 salt without the name the old passphrase wrote under.
	PassphraseOld string
	KeyNameOld    string
}

// Build renders the TF_ENCRYPTION document.
//
// Byte-for-byte the same as the `tf-encryption-env` composite action's output
// for the same inputs; that equivalence is a gate, not a hope (tfenc_test.go
// runs the action's real shell body). Keep them in step, or the gate fails —
// which is the point, because CI and a laptop disagreeing about this document
// means state one of them wrote is unreadable by the other.
func Build(c Config) (string, error) {
	keyName := c.KeyName
	if keyName == "" {
		keyName = DefaultKeyName
	}
	if err := validatePassphrase(c.Passphrase, PassphraseEnv); err != nil {
		return "", err
	}
	if err := validateKeyName(keyName, KeyNameEnv); err != nil {
		return "", err
	}

	var b strings.Builder
	writeKey(&b, keyName, c.Passphrase)
	// method.unencrypted.migrate is referenced by the roots' PHASE-1 fallback
	// (read plaintext, write encrypted). Declared unconditionally so that phase 2
	// — deleting that fallback and setting `enforced = true` — needs no change
	// here. A declared-but-unreferenced method is inert; OpenTofu warns about it
	// at init and encrypts anyway.
	b.WriteString("method \"unencrypted\" \"migrate\" {}\n")

	if c.PassphraseOld == "" {
		if c.KeyNameOld != "" {
			return "", fmt.Errorf("%s is set without %s — a key name alone cannot decrypt anything, and configuring one silently would make a rotation window LOOK open while the old key was never loaded",
				KeyNameOldEnv, PassphraseOldEnv)
		}
		writeState(&b, keyName, "")
		writePlan(&b, keyName)
		return b.String(), nil
	}

	if err := validatePassphrase(c.PassphraseOld, PassphraseOldEnv); err != nil {
		return "", err
	}
	if c.KeyNameOld == "" {
		return "", fmt.Errorf("%s is set, so %s is required — a rotation window must declare the key-provider NAME the previous passphrase wrote state under, because the fallback cannot locate its pbkdf2 salt without it",
			PassphraseOldEnv, KeyNameOldEnv)
	}
	if err := validateKeyName(c.KeyNameOld, KeyNameOldEnv); err != nil {
		return "", err
	}
	if c.KeyNameOld == keyName {
		return "", fmt.Errorf("%s and %s are both %q — the new passphrase MUST use a different name: reusing it feeds the OLD salt to the NEW key and every decrypt fails",
			KeyNameEnv, KeyNameOldEnv, keyName)
	}
	writeKey(&b, c.KeyNameOld, c.PassphraseOld)
	// An ENCRYPTED fallback, which is legal alongside `enforced` (that flag bans
	// method.unencrypted only) — so a rollover never relaxes the posture.
	writeState(&b, keyName, c.KeyNameOld)
	// Plans are ephemeral and always written with the CURRENT key, so they never
	// need the fallback.
	writePlan(&b, keyName)
	return b.String(), nil
}

func writeKey(b *strings.Builder, name, passphrase string) {
	fmt.Fprintf(b, "key_provider \"pbkdf2\" %q {\n  passphrase = %q\n}\n", name, passphrase)
	fmt.Fprintf(b, "method \"aes_gcm\" %q {\n  keys = key_provider.pbkdf2.%s\n}\n", name, name)
}

func writeState(b *strings.Builder, name, fallbackName string) {
	b.WriteString("state {\n")
	fmt.Fprintf(b, "  method = method.aes_gcm.%s\n", name)
	if fallbackName != "" {
		fmt.Fprintf(b, "  fallback {\n    method = method.aes_gcm.%s\n  }\n", fallbackName)
	}
	b.WriteString("}\n")
}

func writePlan(b *strings.Builder, name string) {
	fmt.Fprintf(b, "plan {\n  method = method.aes_gcm.%s\n}\n", name)
}

// validatePassphrase enforces the base64 alphabet and pbkdf2's length floor.
//
// THE CHARSET IS A SECURITY CONTROL, NOT TIDINESS. The passphrase is
// interpolated into an HCL string, so a value carrying a quote or a backslash
// would not merely break parsing — it could CLOSE the string and append
// arbitrary encryption configuration, up to and including swapping in
// method.unencrypted and writing plaintext state that still looks encrypted from
// the outside. Restrict to what the documented `openssl rand -base64 32`
// produces and REJECT anything else rather than escaping it: escaping is a thing
// you can get subtly wrong, and rejecting is not.
func validatePassphrase(p, name string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("%s is not set — the Terraform roots encrypt state at rest, so it is required. Generate one (openssl rand -base64 32), and ESCROW IT OFFLINE: losing it makes every state file unrecoverable. See docs/adr/0007-terraform-state-encryption.md", name)
	}
	if strings.ContainsFunc(p, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '+' || r == '/' || r == '=' || r == '_' || r == '-')
	}) {
		return fmt.Errorf("%s must contain only [A-Za-z0-9+/=_-] — it is interpolated into an HCL string, where a quote or backslash could inject encryption configuration. Regenerate it with 'openssl rand -base64 32'", name)
	}
	if len(p) < MinPassphraseLen {
		return fmt.Errorf("%s is too short: pbkdf2 requires >= %d characters (got %d). Regenerate it with 'openssl rand -base64 32'", name, MinPassphraseLen, len(p))
	}
	return nil
}

// validateKeyName enforces the HCL identifier alphabet, for the same reason as
// the passphrase: the name is interpolated into the config as an identifier.
func validateKeyName(n, envName string) error {
	if n == "" || strings.ContainsFunc(n, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
	}) {
		return fmt.Errorf("%s must be an HCL identifier: [A-Za-z0-9_] only (got %q)", envName, n)
	}
	return nil
}
