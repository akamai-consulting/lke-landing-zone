package main

// lineDiff is in render.go; boolPtrLocal is a package main test helper. Both were
// passengers in env_set_test.go.

import (
	"strings"
	"testing"
)

func boolPtrLocal(b bool) *bool { return &b }

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
