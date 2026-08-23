package render

import (
	"strings"
	"testing"
)

// Tests that followed their subjects rather than the subjects being exported to
// reach them. filepathRel, lineDiff and renderTargets were briefly uppercase so
// package main could call them; none had a non-test caller, which is the test
// applied here before accepting an export.

func TestFilepathRel(t *testing.T) {
	if got := filepathRel("/a/b/cluster", "/a/b/prod.tfvars"); got != "prod.tfvars" {
		t.Errorf("filepathRel = %q, want prod.tfvars", got)
	}
	// Unrelatable paths fall back to dst unchanged.
	if got := filepathRel("rel/dir", "/abs/out"); got != "/abs/out" {
		t.Errorf("filepathRel fallback = %q, want /abs/out", got)
	}
}

// #9: the LCS diff shows scattered changes as separate hunks with a collapse marker.
func TestLineDiffScattered(t *testing.T) {
	old := "x\n1\n2\n3\n4\n5\n6\n7\ny\n"
	new := "X\n1\n2\n3\n4\n5\n6\n7\nY\n"
	d := lineDiff(old, new)
	if !strings.Contains(d, "- x") || !strings.Contains(d, "+ X") || !strings.Contains(d, "- y") || !strings.Contains(d, "+ Y") {
		t.Errorf("both hunks should appear:\n%s", d)
	}
	if !strings.Contains(d, "…") {
		t.Errorf("unchanged run between hunks should collapse to …:\n%s", d)
	}
}

func TestLineDiff(t *testing.T) {
	// Localized change → shows the -/+ pair with context, no truncation note.
	d := lineDiff("a\nb\nc\n", "a\nB\nc\n")
	if !strings.Contains(d, "- b") || !strings.Contains(d, "+ B") {
		t.Errorf("lineDiff missing the change:\n%s", d)
	}
	if strings.Contains(d, "more changes") {
		t.Errorf("small diff should not truncate:\n%s", d)
	}
	// New file (old empty) → all additions.
	if d := lineDiff("", "x\ny\n"); !strings.Contains(d, "+ x") || !strings.Contains(d, "+ y") {
		t.Errorf("new-file diff wrong:\n%s", d)
	}
}

// #4: the components registry view is accurate.

// The golden is only meaningful if the serialization it compares actually
// distinguishes a changed  This asserts the two halves of that: a changed
// spec-derived VALUE shows up (full content), and a changed static file shows up
// too (via its hash), so neither half can drift silently.
