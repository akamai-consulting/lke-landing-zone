package tfvars_test

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tfvars"
)

func TestValueUnquotes(t *testing.T) {
	const src = "region = \"us-ord\"\nbare = 7\nother = \"x\"\n"
	if got := tfvars.Value(src, "region"); got != "us-ord" {
		t.Errorf("Value = %q, want us-ord", got)
	}
	if got := tfvars.Value(src, "bare"); got != "7" {
		t.Errorf("an unquoted value comes back verbatim, got %q", got)
	}
	if got := tfvars.Value(src, "absent"); got != "" {
		t.Errorf("a missing key is the empty string, got %q", got)
	}
}

func TestSetFieldRewritesEveryMatchingLine(t *testing.T) {
	const src = "a = 1\nb = 2\na = 3\n"
	got := tfvars.SetField(src, "a", "9")
	if want := "a = 9\nb = 2\na = 9\n"; got != want {
		t.Errorf("SetField = %q, want %q — EVERY matching line, not the first", got, want)
	}
}

func TestHasKeyIgnoresCommentedAssignments(t *testing.T) {
	if tfvars.HasKey("# region = \"x\"\n", "region") {
		t.Error("a commented-out assignment must not count as present")
	}
	if !tfvars.HasKey("region = \"x\"\n", "region") {
		t.Error("a real assignment must count")
	}
}

// TestReadAndWriteAgreeOnWhatAnAssignmentIs pins a REAL disagreement rather than
// asserting the ideal. Value trims the left side and so accepts an indented
// assignment; SetField and HasKey anchor at column 0 and do not. That means llz
// can read a value it would never rewrite.
//
// It has not bitten anyone because this tree generates these files unindented.
// This test exists so that whoever changes one side is told about the other —
// if you make them agree, this test SHOULD fail, and the fix is to update it
// here with a note, not to delete it.
func TestReadAndWriteAgreeOnWhatAnAssignmentIs(t *testing.T) {
	const indented = "  region = \"us-ord\"\n"

	if got := tfvars.Value(indented, "region"); got != "us-ord" {
		t.Errorf("Value no longer accepts an indented assignment (got %q) — "+
			"if that was deliberate, this test and the package header both need updating", got)
	}
	if tfvars.HasKey(indented, "region") {
		t.Error("HasKey now accepts an indented assignment — it and SetField must move together")
	}
	if got := tfvars.SetField(indented, "region", "\"other\""); got != indented {
		t.Errorf("SetField now rewrites an indented assignment (got %q) — "+
			"it and HasKey must move together, and Value already accepted this line", got)
	}
}
