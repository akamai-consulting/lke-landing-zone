package linode

// timestamp_mutation_test.go closes the civil-date gaps the existing round-trip
// test leaves open. TestCivilDaysRoundtrip checks three dates, all inside one
// era and all after the epoch, and a round trip through a matched pair of
// inverse functions hides any error that both directions share. The era-length
// terms of CivilFromDays' yoe expression are the sharp edge: `doe/146_096` is
// non-zero on exactly ONE day per 400 years (the era's last day, 2000-02-29 for
// the era we live in), so corrupting it is invisible unless a test lands on that
// day. Nothing did.
//
// The oracle here is the standard library rather than the package's own inverse:
// time is a genuinely independent proleptic-Gregorian implementation, which is
// what makes it able to disagree.

import (
	"testing"
	"time"
)

// TestCivilFromDaysMatchesStdlib checks EVERY day across ~5,470 years — a bit
// under fourteen 400-year eras, so every possible doe (0…146_096) occurs many
// times, including the era-boundary day that is the only witness to the
// `doe/146_096` term. It also re-derives the day number with DaysFromCivil, so
// the two functions are pinned individually and as a pair.
func TestCivilFromDaysMatchesStdlib(t *testing.T) {
	for z := int64(-1_000_000); z <= 1_000_000; z++ {
		y, m, d := CivilFromDays(z)
		wy, wm, wd := time.Unix(z*DaySecs, 0).UTC().Date()
		if y != int64(wy) || m != int64(wm) || d != int64(wd) {
			t.Fatalf("CivilFromDays(%d) = %d-%02d-%02d, want %d-%02d-%02d", z, y, m, d, wy, wm, wd)
		}
		if back := DaysFromCivil(y, m, d); back != z {
			t.Fatalf("DaysFromCivil(%d-%02d-%02d) = %d, want %d", y, m, d, back, z)
		}
	}
}

// TestCivilDateNamedEdges spells out the dates the exhaustive sweep covers only
// implicitly, so a failure names the rule that broke instead of a day number:
// the 400-year era boundary, the century that IS a leap year and the two that
// are not, year zero, and pre-Christian (negative, hence pre-`era` adjustment)
// years.
func TestCivilDateNamedEdges(t *testing.T) {
	cases := []struct {
		y, m, d int64
		why     string
	}{
		{2000, 2, 29, "last day of era 4 — the only day doe/146_096 is non-zero"},
		{2000, 3, 1, "first day of era 5"},
		{1900, 2, 28, "1900 is NOT a leap year (century, not divisible by 400)"},
		{1900, 3, 1, "day after the 1900 non-leap February"},
		{2100, 2, 28, "2100 is NOT a leap year either"},
		{1600, 2, 29, "1600 IS a leap year (divisible by 400)"},
		{2024, 2, 29, "ordinary leap year"},
		{0, 1, 1, "year zero, before the March-based year shift"},
		{0, 2, 29, "year zero is a leap year in proleptic Gregorian"},
		{0, 3, 1, "year zero, after the shift — era 0, doe 0"},
		{-1, 12, 31, "negative year: takes the y < 0 era adjustment"},
		{-400, 2, 29, "negative leap century"},
		{-401, 12, 31, "negative year one era back"},
	}
	for _, c := range cases {
		got := DaysFromCivil(c.y, c.m, c.d)
		want := time.Date(int(c.y), time.Month(c.m), int(c.d), 0, 0, 0, 0, time.UTC).Unix() / DaySecs
		if got != want {
			t.Errorf("DaysFromCivil(%d-%02d-%02d) = %d, want %d (%s)", c.y, c.m, c.d, got, want, c.why)
		}
		y, m, d := CivilFromDays(want)
		if y != c.y || m != c.m || d != c.d {
			t.Errorf("CivilFromDays(%d) = %d-%02d-%02d, want %d-%02d-%02d (%s)", want, y, m, d, c.y, c.m, c.d, c.why)
		}
	}
}

// TestFmtLinodeTSPreEpoch drives the Euclidean-division helpers through the
// public formatter: a negative epoch second must floor to the PREVIOUS day with
// a positive time-of-day, not truncate toward zero.
func TestFmtLinodeTSPreEpoch(t *testing.T) {
	cases := map[int64]string{
		-1:             "1969-12-31T23:59:59",
		-86_400:        "1969-12-31T00:00:00",
		-86_401:        "1969-12-30T23:59:59",
		0:              "1970-01-01T00:00:00",
		-2_208_988_800: "1900-01-01T00:00:00",
	}
	for unix, want := range cases {
		if got := FmtLinodeTS(unix); got != want {
			t.Errorf("FmtLinodeTS(%d) = %q, want %q", unix, got, want)
		}
	}
}

// TestParseTSMinutePrecision covers the `len(tp) < 2` gate at both edges. The
// existing tests only ever pass three time fields, so a gate that had drifted to
// `<= 2` — rejecting every HH:MM timestamp — was unobservable.
func TestParseTSMinutePrecision(t *testing.T) {
	// Exactly two fields is VALID: seconds default to zero.
	want := DaysFromCivil(2024, 1, 1)*DaySecs + 12*3600 + 30*60
	if got, ok := ParseTS("2024-01-01T12:30"); !ok || got != want {
		t.Errorf("ParseTS(HH:MM) = (%d, %v), want (%d, true)", got, ok, want)
	}
	if got, ok := ParseTS("2024-01-01T12:30Z"); !ok || got != want {
		t.Errorf("ParseTS(HH:MMZ) = (%d, %v), want (%d, true)", got, ok, want)
	}
	// Fewer than two is not.
	for _, in := range []string{"2024-01-01T12", "2024-01-01T"} {
		if _, ok := ParseTS(in); ok {
			t.Errorf("ParseTS(%q) reported ok, want not-ok", in)
		}
	}
}

// TestFloorDivModSignMatrix covers every sign combination of both operands,
// including the zero dividend the existing table skips. floorDiv/floorMod must
// agree with Euclidean division: the remainder never takes the dividend's sign.
func TestFloorDivModSignMatrix(t *testing.T) {
	cases := []struct {
		a, b, wantDiv, wantMod int64
	}{
		{0, 3, 0, 0},    // zero dividend, positive divisor
		{0, -3, 0, 0},   // zero dividend, negative divisor
		{7, -3, -3, -2}, // negative divisor → floor is more negative, mod follows b's sign
		{-7, -3, 2, -1}, // both negative → exact truncation already floors
		{6, -3, -2, 0},  // negative divisor, exact
		{-6, -3, 2, 0},  // both negative, exact
		{1, 3, 0, 1},
		{-1, 3, -1, 2},
	}
	for _, c := range cases {
		if got := floorDiv(c.a, c.b); got != c.wantDiv {
			t.Errorf("floorDiv(%d,%d) = %d, want %d", c.a, c.b, got, c.wantDiv)
		}
		if got := floorMod(c.a, c.b); got != c.wantMod {
			t.Errorf("floorMod(%d,%d) = %d, want %d", c.a, c.b, got, c.wantMod)
		}
		// The defining identity, checked rather than assumed.
		if q, m := floorDiv(c.a, c.b), floorMod(c.a, c.b); q*c.b+m != c.a {
			t.Errorf("floorDiv/floorMod(%d,%d) violate a = q*b + m (q=%d, m=%d)", c.a, c.b, q, m)
		}
	}
}
