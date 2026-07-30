package main

import (
	"testing"
)

// `<pkg-suffix>=<min>` is split at the LAST '='; only a MISSING '=' is malformed.
// An '=' at offset 0 yields the empty suffix, which is a parse, not an error —
// `eq < 0` is the guard, and `eq <= 0` would reject it.
func TestParseCovThresholdsEmptySuffixIsNotMalformed(t *testing.T) {
	got, err := parseCovThresholds([]string{"=50"})
	if err != nil {
		t.Fatalf("an '=' at offset 0 is a parse, not a malformed threshold: %v", err)
	}
	if len(got) != 1 || got[0].Suffix != "" || got[0].Min != 50 {
		t.Errorf("parsed = %+v, want one entry with an empty suffix and min 50", got)
	}
}

// The +1e-9 epsilon exists so a package sitting EXACTLY on its floor passes. With
// `>` instead of `>=` the epsilon lands precisely on the threshold and the
// package fails — pkg b is 0/2 statements, so pct+1e-9 == 1e-9 bit-for-bit.
func TestEvaluateCoverageEpsilonLandsExactlyOnTheFloor(t *testing.T) {
	thresholds, err := parseCovThresholds([]string{"b=1e-9"})
	if err != nil {
		t.Fatal(err)
	}
	got := evaluateCoverage(sampleProfile, thresholds)
	if len(got) != 1 {
		t.Fatalf("results = %+v", got)
	}
	if !got[0].HasData {
		t.Fatalf("pkg b should have profile data: %+v", got[0])
	}
	if !got[0].OK {
		t.Errorf("0%% coverage against a 1e-9 floor must pass on the epsilon: %+v", got[0])
	}
}

// A profile entry for a file at the module root ("/main.go") has its only '/' at
// offset 0 → package "". That is a real (degenerate) key, not the malformed case
// `slash < 0` guards; `slash <= 0` would drop the entry entirely.
func TestCoverageByPackageRootLevelFileKeepsTheEmptyPackage(t *testing.T) {
	got := coverageByPackage("mode: atomic\n/main.go:1.1,2.2 4 1\n")
	v, ok := got[""]
	if !ok {
		t.Fatalf("root-level file dropped; map = %v", got)
	}
	if v != 100 {
		t.Errorf("root package coverage = %v, want 100", v)
	}
}

// A profile line with no ':' at all IS malformed and must be skipped, as must one
// whose path half has no '/'.
func TestCoverageByPackageSkipsMalformedLines(t *testing.T) {
	got := coverageByPackage("mode: atomic\nnocolon 4 1\nmain.go:1.1,2.2 4 1\n")
	if len(got) != 0 {
		t.Errorf("malformed lines must contribute nothing, got %v", got)
	}
}
