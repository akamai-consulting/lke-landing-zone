package pincoherence

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func writeAnswers(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The guard exists for one shape — two exact release tags that disagree — and
// must stay silent on every other shape, because the non-tag forms are legitimate
// (copier writes a `git describe` string for a non-tag ref) and a guard that
// false-positives on them gets skipped past.
func TestAssertPinCoherence(t *testing.T) {
	cases := []struct {
		name, body string
		wantErr    bool
	}{
		{"agreeing tags pass", "_commit: v0.0.37\nllz_version: v0.0.37\n", false},
		{"skewed tags fail", "_commit: v0.0.33\nllz_version: v0.0.34\n", true},
		// The live gsap-apl shape: whitespace/quoting variations must not smuggle
		// a skew past the comparison.
		{"skew survives quoting", "_commit: \"v0.0.33\"\nllz_version: 'v0.0.34'\n", true},
		// Legitimate non-tag forms — nothing to compare, so nothing to report.
		{"describe form is not compared", "_commit: v0.0.36-5-gabc1234\nllz_version: v0.0.36\n", false},
		{"sha vs tag is not compared", "_commit: 0123456789abcdef0123456789abcdef01234567\nllz_version: v0.0.36\n", false},
		{"missing llz_version is not compared", "_commit: v0.0.36\n", false},
		{"missing _commit is not compared", "llz_version: v0.0.36\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAnswers(t, dir, tc.body)
			err := Assert(dir)
			if tc.wantErr != (err != nil) {
				t.Fatalf("Assert() error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr {
				return
			}
			// The message has to name both values and the remedy — an operator
			// reading it in CI output has no other context for why the run stopped.
			for _, want := range []string{"v0.0.33", "v0.0.34", "llz upgrade --ref"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error message missing %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestAssertPinCoherenceOutsideInstance(t *testing.T) {
	// No .copier-answers.yml at all — the template repo's own checkout, where the
	// lint gate runs and there is no instance pin to check.
	if err := Assert(t.TempDir()); err != nil {
		t.Fatalf("expected a silent pass outside an instance, got %v", err)
	}
}

func TestAssertPinCoherenceUnparseable(t *testing.T) {
	// A malformed answers file is not this guard's business to report — readAnswers
	// errors and the step passes, leaving the diagnosis to the steps that parse it
	// for real. Asserted so a future "return err" here is a deliberate choice.
	dir := t.TempDir()
	writeAnswers(t, dir, "_commit: [unterminated\n")
	if err := Assert(dir); err != nil {
		t.Fatalf("expected a silent pass on an unparseable answers file, got %v", err)
	}
}

// The gate runs END TO END through its own command, which is how the registry
// drives it. Assert had tests; the flag set and the reader it builds did not, and
// the reader is the half that can refuse the file the guard exists to read.
func TestCmdRunsThroughTheFence(t *testing.T) {
	root := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, ".copier-answers.yml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	run := func() error {
		c := Cmd()
		c.SetArgs([]string{"--root", root})
		c.SilenceUsage, c.SilenceErrors = true, true
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		return c.Execute()
	}

	// Skew: the two pins name different releases.
	write("_commit: v1.2.3\nllz_version: v1.2.4\n")
	err := run()
	if err == nil {
		t.Fatal("a skewed pin pair must fail the gate")
	}
	if !strings.Contains(err.Error(), "template pin skew") {
		t.Errorf("the error should name the skew, got: %v", err)
	}

	// Agreement, and the three silent cases: identical pins, a non-release pin,
	// and no answers file at all.
	for _, body := range []string{
		"_commit: v1.2.3\nllz_version: v1.2.3\n",
		"_commit: main\nllz_version: v1.2.3\n",
	} {
		write(body)
		if err := run(); err != nil {
			t.Errorf("%q must pass: %v", body, err)
		}
	}
	if err := os.Remove(filepath.Join(root, ".copier-answers.yml")); err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Errorf("no answers file is not an instance, so not a finding: %v", err)
	}
}

// The reader comes from the DECLARATION. A guard that could mint its own binding
// would be granting itself the capability.
func TestGateBindingComesFromTheDeclaration(t *testing.T) {
	b := gateBinding()
	if b.Kind != extension.Gate {
		t.Errorf("gateBinding returned a %s binding, want a gate", b.Kind)
	}
	var hasRead bool
	for _, g := range b.Grants {
		if g == extension.ReadRepo {
			hasRead = true
		}
	}
	if !hasRead {
		t.Errorf("the gate binding does not declare read-repo, so the guard could not read at all: %v", b.Grants)
	}
}
