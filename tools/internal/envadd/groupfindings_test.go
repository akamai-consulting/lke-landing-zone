package envadd

// TestGroupFindings STAYED with GroupFindings, which is in scaffold.go.
//
// It travelled to internal/configreadiness inside readiness_test.go, but its
// subject is the CLI's presentation layer: collapsing findings that share a token
// AND a remedy so the checklist agrees with the hint it prints. The Finding itself
// is the extension's model; grouping them for a human is not.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/configreadiness"
)

func TestGroupFindings(t *testing.T) {
	// Nine files, one fix. Collapsing by (token, hint) is what makes the checklist
	// agree with the hint it prints: `llz spec set` once, not nine hand edits.
	in := []configreadiness.Finding{
		{File: "a.yaml", Line: 1, Token: configreadiness.InstanceRepoPlaceholder, Hint: "spec fix"},
		{File: "b.yaml", Line: 2, Token: configreadiness.InstanceRepoPlaceholder, Hint: "spec fix"},
		{File: "c.yaml", Line: 3, Token: "REPLACE_ME", Hint: "hand edit"},
		{File: "d.yaml", Line: 4, Token: configreadiness.InstanceRepoPlaceholder, Hint: "hand edit"}, // same token, DIFFERENT remedy
	}
	got := GroupFindings(in)
	if len(got) != 3 {
		t.Fatalf("got %d groups, want 3 (same token with a different remedy must not merge): %+v", len(got), got)
	}
	if got[0].files != 2 || got[0].first.File != "a.yaml" {
		t.Errorf("first group = %+v, want 2 files starting at a.yaml", got[0])
	}
	if got[1].files != 1 || got[1].first.Token != "REPLACE_ME" {
		t.Errorf("second group = %+v, want the single REPLACE_ME", got[1])
	}
	if got[2].files != 1 {
		t.Errorf("third group = %+v, want 1", got[2])
	}
	if GroupFindings(nil) != nil {
		t.Error("no findings should group to nothing")
	}
}

func TestGroupFindingsCountsFilesNotOccurrences(t *testing.T) {
	// One file can carry the same placeholder twice (instance-custom.yaml does),
	// and the count is printed as "(+N more file(s))". Counting occurrences
	// inflated it — the checklist would claim more files than exist.
	in := []configreadiness.Finding{
		{File: "a.yaml", Line: 1, Token: configreadiness.InstanceRepoPlaceholder, Hint: "h"},
		{File: "a.yaml", Line: 9, Token: configreadiness.InstanceRepoPlaceholder, Hint: "h"}, // same file, second line
		{File: "b.yaml", Line: 3, Token: configreadiness.InstanceRepoPlaceholder, Hint: "h"},
	}
	got := GroupFindings(in)
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1: %+v", len(got), got)
	}
	if got[0].files != 2 {
		t.Errorf("files = %d, want 2 (a.yaml and b.yaml — not 3 occurrences)", got[0].files)
	}
	if n := CountFiles(in); n != 2 {
		t.Errorf("CountFiles = %d, want 2", n)
	}
}
