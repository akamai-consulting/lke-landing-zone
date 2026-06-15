package health

import (
	"testing"
	"time"
)

func TestDaysSinceAndUntil(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		t         time.Time
		wantSince int
		wantUntil int
	}{
		{"exactly 10 days ago", now.Add(-10 * 24 * time.Hour), 10, -10},
		{"9.9 days ago floors to 9", now.Add(-10*24*time.Hour + time.Hour), 9, -9},
		{"now", now, 0, 0},
		{"30 days in future", now.Add(30 * 24 * time.Hour), -30, 30},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DaysSince(tc.t, now); got != tc.wantSince {
				t.Errorf("DaysSince = %d, want %d", got, tc.wantSince)
			}
			if got := DaysUntil(tc.t, now); got != tc.wantUntil {
				t.Errorf("DaysUntil = %d, want %d", got, tc.wantUntil)
			}
		})
	}
}

func TestMaxTime(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, ok := MaxTime(nil); ok {
		t.Error("MaxTime(nil) ok = true, want false")
	}
	got, ok := MaxTime([]time.Time{
		base.Add(2 * time.Hour),
		base.Add(48 * time.Hour),
		base.Add(time.Hour),
	})
	if !ok || !got.Equal(base.Add(48*time.Hour)) {
		t.Errorf("MaxTime = (%v, %v), want the latest", got, ok)
	}
}

func TestClassifyRotationAge(t *testing.T) {
	tests := []struct {
		name                string
		age, warn, critical int
		want                Category
	}{
		{"fresh", 10, 35, 90, CatOK},
		{"just under warn", 34, 35, 90, CatOK},
		{"exactly warn", 35, 35, 90, CatWarn},
		{"between warn and critical", 50, 35, 90, CatWarn},
		{"just under critical", 89, 35, 90, CatWarn},
		{"exactly critical", 90, 35, 90, CatFail},
		{"past critical", 120, 35, 90, CatFail},
		{"warn-only fresh (approle)", 50, 100, 0, CatOK},
		{"warn-only at threshold (approle)", 100, 100, 0, CatWarn},
		{"warn-only past threshold (approle)", 365, 100, 0, CatWarn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyRotationAge(tc.age, tc.warn, tc.critical); got != tc.want {
				t.Errorf("ClassifyRotationAge(%d,%d,%d) = %v, want %v", tc.age, tc.warn, tc.critical, got, tc.want)
			}
		})
	}
}

func TestParseExpiryTime(t *testing.T) {
	want := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	ok := []string{
		"2026-12-31 23:59:59 +0000",
		"2026-12-31 23:59:59 UTC",
		"2026-12-31T23:59:59Z",
	}
	for _, s := range ok {
		got, parsed := ParseExpiryTime(s)
		if !parsed {
			t.Errorf("ParseExpiryTime(%q) failed to parse", s)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("ParseExpiryTime(%q) = %v, want %v", s, got.UTC(), want)
		}
	}
	for _, s := range []string{"", "not a date", "yesterday"} {
		if _, parsed := ParseExpiryTime(s); parsed {
			t.Errorf("ParseExpiryTime(%q) parsed, want failure", s)
		}
	}
}

func TestClassifyPATResponse(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	day := func(n int) string {
		return now.Add(time.Duration(n) * 24 * time.Hour).Format("2006-01-02 15:04:05 -0700")
	}
	tests := []struct {
		name      string
		present   bool
		code      int
		header    string
		wantState PATCheckState
		wantCat   Category
	}{
		{"not set", false, 0, "", PATNotSet, CatWarn},
		{"unreachable", true, 0, "", PATUnreachable, CatWarn},
		{"401 invalid", true, 401, day(30), PATInvalid, CatFail},
		{"403 invalid", true, 403, "", PATInvalid, CatFail},
		{"no expiry header (never-expiring)", true, 200, "", PATNoExpiry, CatFail},
		{"unparseable expiry", true, 200, "soon", PATUnparseable, CatWarn},
		{"expired", true, 200, day(-1), PATExpired, CatFail},
		{"over 90-day policy", true, 200, day(120), PATOverPolicy, CatFail},
		{"within warn window", true, 200, day(10), PATWarn, CatWarn},
		{"healthy", true, 200, day(60), PATOK, CatOK},
		{"exactly at max policy is OK", true, 200, day(90), PATOK, CatOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state, _ := ClassifyPATResponse(tc.present, tc.code, tc.header, now, 90, 14)
			if state != tc.wantState {
				t.Errorf("ClassifyPATResponse state = %v, want %v", state, tc.wantState)
			}
			if got := state.Category(); got != tc.wantCat {
				t.Errorf("state.Category() = %v, want %v", got, tc.wantCat)
			}
		})
	}
}
