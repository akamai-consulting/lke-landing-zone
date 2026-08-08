package healthsla

// unit_test.go — two tests that did NOT travel with their subject.
//
// readyCell and schedRegion moved here with the checks that use them, but their
// tests were in package main's coverage_tier1_test.go — a file named for a
// coverage tier rather than for a subject, so nothing about it suggested it held
// assertions about this extension. `go vet` found them, as undefined symbols,
// after the move.
//
// The recorded lesson is that tests travel with the FILE and not the subject.
// This is the same lesson from the other side: a test can fail to travel because
// it was never filed with its subject to begin with. Grep for the moved symbols,
// not just for the moved files.

import "testing"

func TestReadyCell(t *testing.T) {
	if got := readyCell(""); got != "Unknown" {
		t.Errorf("readyCell(\"\") = %q, want Unknown", got)
	}
	if got := readyCell("True"); got != "True" {
		t.Errorf("readyCell(True) = %q, want True", got)
	}
}

func TestSchedRegion(t *testing.T) {
	t.Setenv("REGION", "")
	if got := schedRegion(); got != "cluster" {
		t.Errorf("schedRegion unset = %q, want cluster", got)
	}
	t.Setenv("REGION", "us-ord")
	if got := schedRegion(); got != "us-ord" {
		t.Errorf("schedRegion set = %q, want us-ord", got)
	}
}
