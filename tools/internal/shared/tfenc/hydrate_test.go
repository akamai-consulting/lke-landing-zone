package tfenc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodPassphrase is a legal base64 value at or above the pbkdf2 floor.
const goodPassphrase = "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY"

// clearEnv unsets every variable Hydrate consults, so a developer's own shell
// cannot make a test pass. Without this the suite reports "already present" for
// whatever happens to be exported on the machine running it — which is the
// failure mode this package is about, reproduced in its own tests.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		EnvVar, PassphraseEnv, KeyNameEnv, PassphraseOldEnv, KeyNameOldEnv,
		"TF_STATE_ACCESS_KEY", "TF_STATE_SECRET_KEY", "TF_STATE_ENDPOINT", "TF_STATE_BUCKET",
		"LINODE_API_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_ENDPOINT_URL_S3",
		"LINODE_TOKEN",
	} {
		t.Setenv(k, "")
	}
}

// instanceWith builds a throwaway instance checkout carrying secrets.
func instanceWith(t *testing.T, secrets map[string]string) string {
	t.Helper()
	root := t.TempDir()
	// .copier-answers.yml is one of instanceresolve's markers — the one present
	// from `llz new` onwards, so it is what a real checkout has.
	if err := os.WriteFile(filepath.Join(root, ".copier-answers.yml"), []byte("_src_path: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".llz"), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for k, v := range secrets {
		b.WriteString(k + "=" + v + "\n")
	}
	if err := os.WriteFile(filepath.Join(root, SecretsFile), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func varNamed(l Local, name string) (Var, bool) {
	for _, v := range l.Vars {
		if v.Name == name {
			return v, true
		}
	}
	return Var{}, false
}

// Hydrate runs on the path of EVERY Terraform shell-out, including in CI and in
// unrelated checkouts. Outside an instance it must contribute nothing and report
// no error, or it turns every `tofu` command in the repo into a failure.
func TestHydrateOutsideAnInstanceIsSilentAndEmpty(t *testing.T) {
	clearEnv(t)
	l, err := Hydrate(t.TempDir())
	if err != nil {
		t.Fatalf("outside an instance Hydrate must not error: %v", err)
	}
	if l.Root != "" || len(l.Vars) != 0 || len(l.Missing) != 0 {
		t.Errorf("want an empty result outside an instance, got %+v", l)
	}
}

// The whole design rests on this: hydration only ADDS. If it could overwrite,
// running it unconditionally at the tfbin chokepoint would be unsafe in CI — the
// workflow's own credentials would be replaced by whatever a `.llz` cache on the
// runner happened to hold.
//
// THE ONE EXCEPTION IS NARROWER THAN IT LOOKS, and it is held by
// TestAnAmbientAWSKeyYieldsToTheInstancesOwn: a variable whose cache key is
// spelled DIFFERENTLY (TF_STATE_ACCESS_KEY → AWS_ACCESS_KEY_ID) yields to this
// instance's own value, because the generic name is ambient — every machine with
// an AWS account has it — and its presence is not a statement about this
// instance's Linode state bucket. This test keeps the rule for the two shapes
// where it still holds absolutely: a variable the caller set under the name the
// cache uses for it, and TF_ENCRYPTION, which is BUILT rather than mapped and
// which statepassphrase pins to one key on purpose.
func TestHydrateNeverOverwritesTheEnvironment(t *testing.T) {
	clearEnv(t)
	root := instanceWith(t, map[string]string{
		PassphraseEnv:         goodPassphrase,
		"TF_STATE_ACCESS_KEY": "from-cache",
		"TF_STATE_BUCKET":     "from-cache",
	})
	t.Setenv(EnvVar, "already-configured")
	t.Setenv("TF_STATE_BUCKET", "already-configured")

	l, err := Hydrate(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{EnvVar, "TF_STATE_BUCKET"} {
		if _, ok := varNamed(l, name); ok {
			t.Errorf("%s was already set in the environment and Hydrate proposed to replace it", name)
		}
		if !contains(l.Present, name) {
			t.Errorf("%s should be reported as already present, got Present=%v", name, l.Present)
		}
		if !l.Has(name) {
			t.Errorf("Has(%s) must be true for a variable already in the environment", name)
		}
	}
}

// The `.llz` cache and the consumer spell these differently on purpose
// (TF_STATE_ACCESS_KEY → AWS_ACCESS_KEY_ID). Getting a mapping wrong produces a
// backend that cannot authenticate, reported by the AWS SDK as a credentials
// error that names neither file.
func TestHydrateMapsCacheKeysToWhatTheStackReads(t *testing.T) {
	clearEnv(t)
	root := instanceWith(t, map[string]string{
		PassphraseEnv:         goodPassphrase,
		"TF_STATE_ACCESS_KEY": "ak",
		"TF_STATE_SECRET_KEY": "sk",
		"TF_STATE_ENDPOINT":   "https://us-east-1.linodeobjects.com",
		"TF_STATE_BUCKET":     "state-bucket",
		"LINODE_API_TOKEN":    "linode-pat",
	})
	l, err := Hydrate(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Missing) != 0 {
		t.Errorf("everything was cached, yet Hydrate reports missing: %+v", l.Missing)
	}
	for name, want := range map[string]string{
		"AWS_ACCESS_KEY_ID":     "ak",
		"AWS_SECRET_ACCESS_KEY": "sk",
		"AWS_ENDPOINT_URL_S3":   "https://us-east-1.linodeobjects.com",
		"TF_STATE_BUCKET":       "state-bucket",
		"LINODE_TOKEN":          "linode-pat",
	} {
		v, ok := varNamed(l, name)
		if !ok {
			t.Errorf("%s was not resolved", name)
			continue
		}
		if v.Value != want {
			t.Errorf("%s = %q, want %q", name, v.Value, want)
		}
	}
	enc, ok := varNamed(l, EnvVar)
	if !ok {
		t.Fatalf("%s was not built from the cached passphrase", EnvVar)
	}
	if !strings.Contains(enc.Value, `key_provider "pbkdf2" "`+DefaultKeyName+`"`) {
		t.Errorf("%s was built under the wrong key name — state written under %q is unreadable "+
			"by any other:\n%s", EnvVar, DefaultKeyName, enc.Value)
	}
}

// Hydrate runs before commands that do not need any of this (`tofu fmt`,
// `tofu validate`). An absent value must be REPORTED, not fatal — but it must be
// reported against the key the operator can actually set.
func TestHydrateReportsMissingSourceKeysNotConsumerNames(t *testing.T) {
	clearEnv(t)
	root := instanceWith(t, map[string]string{PassphraseEnv: goodPassphrase})
	l, err := Hydrate(root)
	if err != nil {
		t.Fatalf("an incomplete cache is a normal state, not an error: %v", err)
	}
	got := map[string]string{}
	for _, m := range l.Missing {
		got[m.Key] = m.Provides
	}
	if got["TF_STATE_ACCESS_KEY"] != "AWS_ACCESS_KEY_ID" {
		t.Errorf("missing values must name the key the operator sets (TF_STATE_ACCESS_KEY), "+
			"not the one the SDK reads; got %+v", l.Missing)
	}
	if _, ok := got["AWS_ACCESS_KEY_ID"]; ok {
		t.Error("reported AWS_ACCESS_KEY_ID as the thing to set — that sends the operator " +
			"looking for an AWS account they do not have")
	}
}

// A cached passphrase that cannot produce a valid document is PRESENT AND WRONG,
// and the operator believes it is configured. Silently skipping it would surface
// later as OpenTofu's own unhelpful "Invalid expression" — the exact message this
// package exists to stop people seeing.
func TestHydrateFailsLoudlyOnMalformedCachedMaterial(t *testing.T) {
	clearEnv(t)
	root := instanceWith(t, map[string]string{PassphraseEnv: `x" } method "unencrypted" "z" {}`})
	_, err := Hydrate(root)
	if err == nil {
		t.Fatal("an injectable cached passphrase must be an error, not a skipped variable")
	}
	if !strings.Contains(err.Error(), SecretsFile) {
		t.Errorf("the error must name the file to fix, got: %v", err)
	}
}

// A rotation window in the cache must produce the SAME two-key document CI
// builds, or a local `tofu state pull` cannot read state the previous key wrote.
func TestHydrateCarriesARotationWindow(t *testing.T) {
	clearEnv(t)
	root := instanceWith(t, map[string]string{
		PassphraseEnv:    goodPassphrase,
		KeyNameEnv:       "llz_g2",
		PassphraseOldEnv: "b2xkLXBhc3NwaHJhc2UtdmFsdWUtaGVyZQ==",
		KeyNameOldEnv:    "llz",
	})
	l, err := Hydrate(root)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := varNamed(l, EnvVar)
	if !strings.Contains(enc.Value, "fallback") || !strings.Contains(enc.Value, "method.aes_gcm.llz\n") {
		t.Errorf("a cached rotation window must emit an encrypted fallback to the old key:\n%s", enc.Value)
	}
}

// Exports is fed to `eval`, and TF_ENCRYPTION is a multi-line HCL document. A
// quoting slip does not produce a syntax error the operator can see — it produces
// a shell that silently exports a truncated document, and OpenTofu then reports
// the same "Invalid expression" as having no document at all.
func TestExportsQuoteMultilineAndEmbeddedQuotes(t *testing.T) {
	l := Local{Vars: []Var{{Name: "X", Value: "line1\nline2 'quoted'"}}}
	got := l.Exports()
	if !strings.HasPrefix(got, "export X='line1\nline2 ") {
		t.Errorf("multi-line values must stay inside one single-quoted string, got:\n%s", got)
	}
	if !strings.Contains(got, `'\''quoted'\''`) {
		t.Errorf("an embedded single quote must be escaped as '\\'', got:\n%s", got)
	}
}

// TestAnAmbientAWSKeyYieldsToTheInstancesOwn is the gate on the one exception to
// "never overwrite", and it exists because the rule without it broke every
// documented `llz tofu` flow on any machine that has AWS credentials.
//
// AWS_ACCESS_KEY_ID is generic and ambient — anyone with an AWS account exports
// it, and its presence says nothing about which credential belongs to THIS
// instance's Linode Object Storage state bucket. Leaving it alone handed the
// operator's AWS key to Linode and produced `InvalidAccessKeyId`, with nothing
// naming the conflict. Found by running the release's own printed remedy on a
// real workstation.
func TestAnAmbientAWSKeyYieldsToTheInstancesOwn(t *testing.T) {
	clearEnv(t)
	root := instanceWith(t, map[string]string{
		PassphraseEnv:         "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY",
		"TF_STATE_ACCESS_KEY": "instance-key",
		"TF_STATE_SECRET_KEY": "instance-secret",
		"TF_STATE_BUCKET":     "instance-bucket",
	})
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA-SOMEONES-REAL-AWS")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("TF_STATE_BUCKET", "")

	l, err := Hydrate(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Value("AWS_ACCESS_KEY_ID"); got != "instance-key" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want this instance's own key — an ambient AWS credential "+
			"is for a different cloud and cannot reach a Linode state bucket", got)
	}
	// SAID OUT LOUD. Replacing something the operator exported is right here and
	// still the most surprising thing this does; a silent override is how a later
	// credential error becomes inexplicable.
	if len(l.Overrode) != 1 || l.Overrode[0].Name != "AWS_ACCESS_KEY_ID" || l.Overrode[0].From != "TF_STATE_ACCESS_KEY" {
		t.Errorf("the override must be reported by name, got %v", l.Overrode)
	}
	if strings.Contains(ResolvedNoteFor(l), "left alone") {
		t.Errorf("the note still promises nothing was overwritten on a run that overwrote something:\n%s",
			ResolvedNoteFor(l))
	}
}

// The LLZ-spelled name still wins outright: an operator who deliberately pointed
// this instance at another backend keeps that, because they said so under the name
// that means it.
func TestAnExplicitLLZSpelledValueIsNeverOverridden(t *testing.T) {
	clearEnv(t)
	root := instanceWith(t, map[string]string{
		PassphraseEnv:         "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY",
		"TF_STATE_ACCESS_KEY": "cached-key",
	})
	t.Setenv("TF_STATE_ACCESS_KEY", "deliberate-key")
	t.Setenv("AWS_ACCESS_KEY_ID", "")

	l, err := Hydrate(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Value("AWS_ACCESS_KEY_ID"); got != "deliberate-key" {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want the exported TF_STATE_ACCESS_KEY", got)
	}
}

// A name the operator sets under its OWN spelling is left alone, override or not —
// TF_STATE_BUCKET maps to itself, so there is no ambient/instance ambiguity to
// resolve and the original rule stands.
func TestASelfNamedVariableIsStillNeverOverwritten(t *testing.T) {
	clearEnv(t)
	root := instanceWith(t, map[string]string{
		PassphraseEnv:     "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY",
		"TF_STATE_BUCKET": "cached-bucket",
	})
	t.Setenv("TF_STATE_BUCKET", "operators-bucket")

	l, err := Hydrate(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Value("TF_STATE_BUCKET"); got != "operators-bucket" {
		t.Errorf("TF_STATE_BUCKET = %q, want the operator's — they set it under its own name", got)
	}
	if len(l.Overrode) != 0 {
		t.Errorf("nothing was overridden, got %v", l.Overrode)
	}
}

// The CI property this whole design rests on, restated against the new rule: the
// workflow sets AWS_ACCESS_KEY_ID *from* TF_STATE_ACCESS_KEY, so the two agree and
// there is nothing to override. (There is also no `.llz` cache in CI, which makes
// it a no-op twice over.)
func TestMatchingValuesAreNotReportedAsAnOverride(t *testing.T) {
	clearEnv(t)
	root := instanceWith(t, map[string]string{
		PassphraseEnv:         "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY",
		"TF_STATE_ACCESS_KEY": "same-key",
	})
	t.Setenv("AWS_ACCESS_KEY_ID", "same-key")

	l, err := Hydrate(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Overrode) != 0 {
		t.Errorf("the values agree; there is nothing to override or report, got %v", l.Overrode)
	}
}
