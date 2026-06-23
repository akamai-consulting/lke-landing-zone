package main

import (
	"os"
	"strings"
	"testing"
)

// A minimal two-package coverprofile: pkg `a` is 3/4 covered (75%), pkg `b` is
// 0/2 (0%).
const sampleProfile = `mode: atomic
example.com/m/a/f.go:1.1,2.2 2 1
example.com/m/a/g.go:1.1,2.2 1 1
example.com/m/a/g.go:3.1,4.2 1 0
example.com/m/b/h.go:1.1,2.2 2 0
`

func TestCoverageByPackage(t *testing.T) {
	got := coverageByPackage(sampleProfile)
	if v := got["example.com/m/a"]; v < 74.9 || v > 75.1 {
		t.Errorf("pkg a = %.1f, want 75.0", v)
	}
	if v := got["example.com/m/b"]; v != 0 {
		t.Errorf("pkg b = %.1f, want 0.0", v)
	}
}

func TestCoverageForSuffix(t *testing.T) {
	byPkg := coverageByPackage(sampleProfile)
	if v, ok := coverageForSuffix(byPkg, "a"); !ok || v < 74.9 {
		t.Errorf("suffix a = (%.1f, %v), want (75.0, true)", v, ok)
	}
	if _, ok := coverageForSuffix(byPkg, "nope"); ok {
		t.Error("suffix nope should have no data")
	}
}

func TestEvaluateCoverage(t *testing.T) {
	thresholds := []covThreshold{
		{Suffix: "a", MinStr: "70", Min: 70}, // 75 >= 70 → ok
		{Suffix: "a", MinStr: "80", Min: 80}, // 75 < 80 → fail
		{Suffix: "b", MinStr: "0", Min: 0},   // 0 >= 0 → ok (exactly at floor)
		{Suffix: "gone", MinStr: "50", Min: 50},
	}
	got := evaluateCoverage(sampleProfile, thresholds)

	if !got[0].OK || !got[0].HasData {
		t.Errorf("a>=70 = %+v, want OK", got[0])
	}
	if got[1].OK {
		t.Errorf("a>=80 should fail, got %+v", got[1])
	}
	if !got[2].OK {
		t.Errorf("b>=0 should pass at the floor, got %+v", got[2])
	}
	if got[3].HasData || got[3].OK {
		t.Errorf("missing package should be no-data failure, got %+v", got[3])
	}
}

func TestParseCovThresholds(t *testing.T) {
	got, err := parseCovThresholds([]string{"cmd/llz=48", "internal/cli=95"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || got[0].Suffix != "cmd/llz" || got[0].Min != 48 {
		t.Errorf("parsed = %+v", got)
	}
	if _, err := parseCovThresholds([]string{"noequals"}); err == nil {
		t.Error("expected error for malformed threshold")
	}
	if _, err := parseCovThresholds([]string{"pkg=notanumber"}); err == nil {
		t.Error("expected error for non-numeric min")
	}
}

func TestRunCheckCoverage(t *testing.T) {
	dir := t.TempDir()
	profile := dir + "/cover.out"
	if err := os.WriteFile(profile, []byte(sampleProfile), 0o644); err != nil {
		t.Fatal(err)
	}

	// All floors met.
	var out strings.Builder
	if err := runCheckCoverage(profile, []string{"a=70", "b=0"}, &out); err != nil {
		t.Errorf("expected pass, got %v", err)
	}
	if !strings.Contains(out.String(), "all gated packages meet") {
		t.Errorf("missing success line: %q", out.String())
	}

	// One floor breached → error.
	out.Reset()
	if err := runCheckCoverage(profile, []string{"a=90"}, &out); err == nil {
		t.Error("expected failure for a=90")
	}

	// Missing profile.
	if err := runCheckCoverage(dir+"/nope.out", []string{"a=1"}, &out); err == nil {
		t.Error("expected error for missing profile")
	}

	// Missing --profile flag.
	if err := runCheckCoverage("", []string{"a=1"}, &out); err == nil {
		t.Error("expected error for empty --profile")
	}
}
