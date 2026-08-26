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
func TestHydrateNeverOverwritesTheEnvironment(t *testing.T) {
	clearEnv(t)
	root := instanceWith(t, map[string]string{
		PassphraseEnv:         goodPassphrase,
		"TF_STATE_ACCESS_KEY": "from-cache",
	})
	t.Setenv(EnvVar, "already-configured")
	t.Setenv("AWS_ACCESS_KEY_ID", "already-configured")

	l, err := Hydrate(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{EnvVar, "AWS_ACCESS_KEY_ID"} {
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
