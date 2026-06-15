package linode

import "testing"

func TestCivilDaysRoundtrip(t *testing.T) {
	// The Unix epoch anchors the civil-date arithmetic.
	if got := DaysFromCivil(1970, 1, 1); got != 0 {
		t.Errorf("DaysFromCivil(1970,1,1) = %d, want 0", got)
	}
	cases := [][3]int64{{1970, 1, 1}, {2024, 2, 29}, {2099, 12, 31}}
	for _, c := range cases {
		z := DaysFromCivil(c[0], c[1], c[2])
		y, m, d := CivilFromDays(z)
		if y != c[0] || m != c[1] || d != c[2] {
			t.Errorf("CivilFromDays(DaysFromCivil(%v)) = %d-%d-%d, want %v", c, y, m, d, c)
		}
	}
}

func TestFmtLinodeTS(t *testing.T) {
	// 2024-01-01T00:00:00 UTC = 1704067200
	if got := FmtLinodeTS(1_704_067_200); got != "2024-01-01T00:00:00" {
		t.Errorf("FmtLinodeTS(1704067200) = %q, want 2024-01-01T00:00:00", got)
	}
	// Leap-day check.
	leap := DaysFromCivil(2024, 2, 29)*DaySecs + 12*3600 + 34*60 + 56
	if got := FmtLinodeTS(leap); got != "2024-02-29T12:34:56" {
		t.Errorf("FmtLinodeTS(leap) = %q, want 2024-02-29T12:34:56", got)
	}
}

func TestParseTSRoundtrip(t *testing.T) {
	original := "2026-05-27T18:42:13"
	parsed, ok := ParseTS(original)
	if !ok {
		t.Fatalf("ParseTS(%q) failed", original)
	}
	if got := FmtLinodeTS(parsed); got != original {
		t.Errorf("round trip = %q, want %q", got, original)
	}

	// 2024-01-01T00:00:00 UTC = 1704067200, with and without the trailing Z.
	for _, in := range []string{"2024-01-01T00:00:00", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00.123"} {
		if got, ok := ParseTS(in); !ok || got != 1_704_067_200 {
			t.Errorf("ParseTS(%q) = (%d, %v), want (1704067200, true)", in, got, ok)
		}
	}
	// Empty and malformed inputs report not-ok rather than a bogus epoch.
	if _, ok := ParseTS(""); ok {
		t.Error("ParseTS(\"\") should report not-ok")
	}
	if _, ok := ParseTS("not-a-date"); ok {
		t.Error(`ParseTS("not-a-date") should report not-ok`)
	}
}
