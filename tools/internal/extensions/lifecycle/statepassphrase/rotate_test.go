package statepassphrase

import (
	"errors"
	"strings"
	"testing"
)

func withRolloverSeams(t *testing.T, rekey, verify func(string) error, present map[string]bool) {
	t.Helper()
	ork, ov, oe := rekeyStateFn, verifyStateFn, statePassphraseRootExists
	rekeyStateFn = func(dir string) error { return rekey(dir) }
	verifyStateFn = func(dir, _ string) error { return verify(dir) }
	statePassphraseRootExists = func(dir string) bool { return present[dir] }
	t.Cleanup(func() { rekeyStateFn, verifyStateFn, statePassphraseRootExists = ork, ov, oe })
}

func rotationWindowEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TF_ENCRYPTION", `key_provider "pbkdf2" "n" {}
state {
  method = method.aes_gcm.n
  fallback { method = method.aes_gcm.o }
}`)
	t.Setenv("TF_STATE_ENCRYPTION_PASSPHRASE", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	t.Setenv("TF_STATE_ENCRYPTION_KEY_NAME", "llz_g2")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
}

func allRoots() map[string]bool {
	m := map[string]bool{}
	for _, r := range statePassphraseRoots {
		m["terraform/"+r] = true
	}
	return m
}

// The command must refuse to run outside a rotation window. Without a fallback
// `state push` re-writes under the SAME key and the verify passes trivially —
// which would report a successful rollover that never happened and license
// deleting a passphrase state still depends on.
func TestRotateStatePassphraseRefusesWithoutAFallback(t *testing.T) {
	t.Setenv("TF_ENCRYPTION", `state { method = method.aes_gcm.n }`)
	t.Setenv("TF_ENCRYPTION_NEW_ONLY", `state { method = method.aes_gcm.n }`)
	err := RunRotate(true, "terraform")
	if err == nil || !strings.Contains(err.Error(), "no fallback") {
		t.Fatalf("want a no-fallback refusal, got %v", err)
	}
}

// The verify pass is the entire safety property. Without the new-key-alone
// config it would run against the window config, decrypt via the FALLBACK, and
// report success for a root still on the old key.
func TestRotateStatePassphraseRequiresTheNewPassphrase(t *testing.T) {
	t.Setenv("TF_ENCRYPTION", `state { fallback { method = method.aes_gcm.o } }`)
	t.Setenv("TF_STATE_ENCRYPTION_PASSPHRASE", "")
	err := RunRotate(true, "terraform")
	if err == nil || !strings.Contains(err.Error(), "TF_STATE_ENCRYPTION_PASSPHRASE") {
		t.Fatalf("want a refusal naming the passphrase, got %v", err)
	}
}

// The new-key-only config is what makes "verified" mean "safe to delete the old
// passphrase". It must carry the new key and NO fallback — a fallback would let a
// root still on the old key pass.
func TestBuildNewKeyOnlyEncryption(t *testing.T) {
	got, err := buildNewKeyOnlyEncryption("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", "llz_g2")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(got, "fallback") {
		t.Error("the verify config MUST NOT carry a fallback — it would decrypt old-key state and pass")
	}
	for _, want := range []string{
		`key_provider "pbkdf2" "llz_g2"`,
		`method "aes_gcm" "llz_g2"`,
		"method = method.aes_gcm.llz_g2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if def, _ := buildNewKeyOnlyEncryption("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ""); !strings.Contains(def, `"llz"`) {
		t.Error("empty key name should default to llz")
	}
}

// The passphrase and key name are interpolated into HCL. A quote or backslash
// could close the string and append encryption configuration — e.g. swapping in
// method.unencrypted, which would silently write PLAINTEXT state.
func TestBuildNewKeyOnlyEncryptionRejectsInjection(t *testing.T) {
	for _, bad := range []struct{ pass, name, why string }{
		{`x" } method "unencrypted" "z" {}`, "llz", "quote in passphrase"},
		{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", `llz" }`, "quote in key name"},
		{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "llz-g2", "hyphen is not an HCL identifier"},
		{"has spaces in it aaaaaaaaaaaaaaa", "llz", "space in passphrase"},
	} {
		if _, err := buildNewKeyOnlyEncryption(bad.pass, bad.name); err == nil {
			t.Errorf("%s: must be rejected", bad.why)
		}
	}
}

func TestRotateStatePassphraseAllRootsVerify(t *testing.T) {
	rotationWindowEnv(t)
	var rekeyed []string
	withRolloverSeams(t,
		func(d string) error { rekeyed = append(rekeyed, d); return nil },
		func(string) error { return nil },
		allRoots())
	if err := RunRotate(true, "terraform"); err != nil {
		t.Fatalf("rollover: %v", err)
	}
	if len(rekeyed) != len(statePassphraseRoots) {
		t.Errorf("re-keyed %d roots, want all %d — a missed root strands state on a discarded passphrase",
			len(rekeyed), len(statePassphraseRoots))
	}
}

// One failed root must fail the COMMAND, because the workflow gates deleting the
// old passphrase on its exit status. Reporting success here destroys the only
// key that can read the root that did not roll over.
func TestRotateStatePassphraseFailsWhenAnyRootDoesNot(t *testing.T) {
	rotationWindowEnv(t)
	withRolloverSeams(t,
		func(string) error { return nil },
		func(d string) error {
			if strings.HasSuffix(d, "/databases") {
				return errors.New("no decryption key available")
			}
			return nil
		},
		allRoots())
	err := RunRotate(true, "terraform")
	if err == nil {
		t.Fatal("a root that fails verification MUST fail the command — the old passphrase is still load-bearing")
	}
	if !strings.Contains(err.Error(), "MUST be retained") {
		t.Errorf("error should tell the operator to keep the old passphrase, got: %v", err)
	}
}

// A re-key that succeeds but does not verify is the dangerous middle state: the
// write happened, so the root may be readable by neither key if the new config
// is wrong. It must count as a failure, not a success.
func TestRotateStatePassphraseRekeyedButUnverifiedIsAFailure(t *testing.T) {
	rotationWindowEnv(t)
	withRolloverSeams(t,
		func(string) error { return nil },
		func(string) error { return errors.New("decryption failed for all attempted") },
		allRoots())
	if err := RunRotate(true, "terraform"); err == nil {
		t.Fatal("re-keyed-but-unverified must fail")
	}
}

// An instance without the databases root is normal; skipping is not a failure.
func TestRotateStatePassphraseSkipsAbsentRoots(t *testing.T) {
	rotationWindowEnv(t)
	present := allRoots()
	delete(present, "terraform/databases")
	var rekeyed []string
	withRolloverSeams(t,
		func(d string) error { rekeyed = append(rekeyed, d); return nil },
		func(string) error { return nil },
		present)
	if err := RunRotate(true, "terraform"); err != nil {
		t.Fatalf("absent root should not fail the rollover: %v", err)
	}
	for _, d := range rekeyed {
		if strings.HasSuffix(d, "/databases") {
			t.Error("re-keyed a root that is not present")
		}
	}
}

// Dry run is the default and must touch nothing.
func TestRotateStatePassphraseDryRunTouchesNothing(t *testing.T) {
	rotationWindowEnv(t)
	withRolloverSeams(t,
		func(string) error { t.Error("dry run must not re-key"); return nil },
		func(string) error { t.Error("dry run must not verify"); return nil },
		allRoots())
	if err := RunRotate(false, "terraform"); err != nil {
		t.Fatalf("dry run: %v", err)
	}
}

// lastLines keeps an OpenTofu error legible without pasting its whole banner.
func TestLastLines(t *testing.T) {
	if got := lastLines("a\nb\nc\nd\ne", 2); got != "d\ne" {
		t.Errorf("lastLines = %q, want %q", got, "d\ne")
	}
	if got := lastLines("only", 3); got != "only" {
		t.Errorf("short input should pass through, got %q", got)
	}
}
