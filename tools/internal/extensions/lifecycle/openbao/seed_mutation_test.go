package openbao

// Mutation-test gap closure for ci_bao_seed.go: the masking that keeps freshly
// minted credentials out of CI logs, the on-missing reporting the bootstrap gate
// reads, and the deferred-failure write that gate depends on.

import (
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
)

// MaskGHALines must emit one ::add-mask:: per NON-BLANK line. Masking a blank or
// whitespace-only line is worse than useless: Actions would then redact matching
// whitespace across the whole log, and — the real hazard — a mask registered for
// the wrong lines leaves the actual secret lines unmasked.
func TestMaskGHALinesMasksOnlyNonBlankLines(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	out := captureStdout(t, func() {
		ghaout.MaskLines("-----BEGIN KEY-----\n\n   \nsecret-body\n-----END KEY-----\n")
	})
	want := "::add-mask::-----BEGIN KEY-----\n::add-mask::secret-body\n::add-mask::-----END KEY-----\n"
	if out != want {
		t.Errorf("MaskGHALines output:\n%q\nwant:\n%q", out, want)
	}
}

// Literals are workflow-visible non-secrets (usernames like "admin"); masking one
// corrupts every log line containing that word. Everything else IS a credential
// and must be masked before any later output can echo it.
func TestRunCIBaoSeedMasksResolvedSecretsButNotLiterals(t *testing.T) {
	stubBaoSeedKV(t, "", "")
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("SEED_SECRET_VALUE", "s3cr3t-token")
	withGHASummaryFile(t)
	withGHAEnvFile(t)

	var err error
	out := captureStdout(t, func() {
		err = RunSeed(Opts{
			Path:       "secret/x/y",
			FieldSpecs: []string{"token=env:SEED_SECRET_VALUE", "username=literal:admin"},
			OnMissing:  "error",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "::add-mask::s3cr3t-token") {
		t.Errorf("the resolved credential was never masked:\n%s", out)
	}
	if strings.Contains(out, "::add-mask::admin") {
		t.Errorf("a literal must NOT be masked — it would redact every log line containing it:\n%s", out)
	}
}

// --seeded-message replaces the default line; the default only fills in when the
// caller supplied none. The workflow uses it to say WHICH credential landed.
func TestRunCIBaoSeedSeededMessage(t *testing.T) {
	for _, tc := range []struct{ name, msg, want, notWant string }{
		{name: "custom", msg: "grafana admin seeded (fresh).", want: "grafana admin seeded (fresh).", notWant: "secret/x/y seeded."},
		{name: "default", msg: "", want: "secret/x/y seeded.", notWant: "(fresh)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubBaoSeedKV(t, "", "")
			t.Setenv("OPENBAO_ROOT_TOKEN", "root")
			withGHASummaryFile(t)
			var err error
			out := captureStdout(t, func() {
				err = RunSeed(Opts{
					Path:          "secret/x/y",
					FieldSpecs:    []string{"username=literal:admin"},
					OnMissing:     "error",
					SeededMessage: tc.msg,
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("stdout %q must contain %q", out, tc.want)
			}
			if strings.Contains(out, tc.notWant) {
				t.Errorf("stdout %q must not contain %q", out, tc.notWant)
			}
		})
	}
}

// The standby note override replaces the base notes only when it actually has
// content. A standby step that declares an on-missing override but no bespoke
// notes must still explain itself in the job summary — swapping in an EMPTY note
// list leaves the operator with a silently skipped credential.
func TestRunCIBaoSeedStandbyKeepsBaseNotesWhenNoStandbyNotes(t *testing.T) {
	stubBaoSeedKV(t, "", "")
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("HA_ROLE", "standby")
	t.Setenv("UNSET_SEED_VAR", "")
	sum := withGHASummaryFile(t)
	withGHAEnvFile(t)

	err := RunSeed(Opts{
		Path:             "secret/x/y",
		FieldSpecs:       []string{"token=env:UNSET_SEED_VAR"},
		OnMissing:        "error",
		OnMissingStandby: "skip",
		MissingNotes:     []string{"base note explaining the skip"},
		// missingNotesStandby deliberately empty
	})
	if err != nil {
		t.Fatalf("missing inputs must exit 0: %v", err)
	}
	b, _ := os.ReadFile(sum)
	if !strings.Contains(string(b), "base note explaining the skip") {
		t.Errorf("summary %q lost the base note when the standby override carried none", b)
	}
}

// --missing-annotation replaces the derived one; with none supplied the derived
// text is what names the missing input. Getting this backwards means the ::error::
// annotation either says nothing useful or drops the caller's wording entirely.
func TestRunCIBaoSeedMissingAnnotationSelection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		annotations []string
		want        string
		notWant     string
	}{
		{name: "custom replaces the default", annotations: []string{"HARBOR_ADMIN not ready yet"},
			want: "::warning::HARBOR_ADMIN not ready yet", notWant: "not seeded"},
		{name: "default derived from the missing names", annotations: nil,
			want: "::warning::UNSET_SEED_VAR not set — secret/x/y not seeded", notWant: "not ready yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubBaoSeedKV(t, "", "")
			t.Setenv("OPENBAO_ROOT_TOKEN", "root")
			t.Setenv("HA_ROLE", "")
			t.Setenv("UNSET_SEED_VAR", "")
			withGHASummaryFile(t)
			withGHAEnvFile(t)

			var err error
			errOut := captureStderr(t, func() {
				err = RunSeed(Opts{
					Path:               "secret/x/y",
					FieldSpecs:         []string{"token=env:UNSET_SEED_VAR"},
					OnMissing:          "warn",
					MissingAnnotations: tc.annotations,
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr %q must contain %q", errOut, tc.want)
			}
			if strings.Contains(errOut, tc.notWant) {
				t.Errorf("stderr %q must not contain %q", errOut, tc.notWant)
			}
		})
	}
}

// on-missing=error defers the failure by writing BOOTSTRAP_ERRORS=true for the
// job's final gate. If that write fails the run must FAIL LOUD: swallowing it
// leaves an exit-0 step whose gate never fires, so the bootstrap reports success
// with an unseeded credential.
func TestRunCIBaoSeedUnwritableGHAEnvIsFatal(t *testing.T) {
	stubBaoSeedKV(t, "", "")
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("HA_ROLE", "")
	t.Setenv("UNSET_SEED_VAR", "")
	withGHASummaryFile(t)
	// A directory can be created but never opened for append — the write fails.
	t.Setenv("GITHUB_ENV", t.TempDir())

	var err error
	captureStderr(t, func() {
		err = RunSeed(Opts{
			Path:       "secret/x/y",
			FieldSpecs: []string{"token=env:UNSET_SEED_VAR"},
			OnMissing:  "error",
		})
	})
	if err == nil {
		t.Fatal("a failed BOOTSTRAP_ERRORS write must fail the run — the deferred-failure gate is the only thing that would have caught it")
	}
}
