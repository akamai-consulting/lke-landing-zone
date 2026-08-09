package pincoherence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
