package cigate

import "testing"

// A workflow command ends at the first raw newline, so an un-escaped multi-line
// annotation keeps its headline and drops its evidence into plain log text.
func TestAnnotationSurvivesAsOneWorkflowCommand(t *testing.T) {
	got := Annotation("could not list versions\n  401 Invalid Token\n  check the PAT scope")
	for _, bad := range []string{"\n", "\r"} {
		if contains(got, bad) {
			t.Errorf("Annotation left a raw %q, which truncates the annotation: %q", bad, got)
		}
	}
	if !contains(got, "401 Invalid Token") {
		t.Errorf("the reason must survive into the annotation: %q", got)
	}
	if got != "could not list versions%0A  401 Invalid Token%0A  check the PAT scope" {
		t.Errorf("unexpected encoding: %q", got)
	}
}

// `%` is escaped FIRST, or the escapes get re-escaped. A Linode error carrying a
// literal percent is the case that finds this.
func TestAnnotationEscapesPercentBeforeNewlines(t *testing.T) {
	if got := Annotation("100% full\nretry"); got != "100%25 full%0Aretry" {
		t.Errorf("Annotation(%q) = %q", "100% full\nretry", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The escaping is worth it ONLY where something reads it. `llz ci` verbs run on
// laptops and in plain CI logs too, and there `%0A` in place of the remedy is its
// own way of hiding the reason — the exact failure the annotation was added to
// prevent, in the other direction.
func TestWarningOnlyEncodesWhereAnAnnotationIsRead(t *testing.T) {
	msg := "could not list versions\n  401 Invalid Token"

	t.Setenv("GITHUB_ACTIONS", "true")
	got := Warning(msg)
	if got != "::warning::could not list versions%0A  401 Invalid Token" {
		t.Errorf("under Actions this must be one workflow command: %q", got)
	}

	t.Setenv("GITHUB_ACTIONS", "")
	got = Warning(msg)
	if contains(got, "%0A") || contains(got, "::warning::") {
		t.Errorf("outside Actions the encoding is just noise in front of the reason: %q", got)
	}
	if !contains(got, "401 Invalid Token") || !contains(got, "\n") {
		t.Errorf("the reason must stay readable, on its own line: %q", got)
	}
}
