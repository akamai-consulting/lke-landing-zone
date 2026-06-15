package linode

import (
	"fmt"
	"strconv"
	"strings"
)

// Chrono-free civil-date arithmetic (Howard Hinnant's algorithms), so the
// rotators format and parse Linode API timestamps without a calendar/time-zone
// dependency — matching the sibling linode-cred-audit tool exactly.

// DaysFromCivil converts a civil date (y-m-d) to days since the Unix epoch.
func DaysFromCivil(y, m, d int64) int64 {
	if m <= 2 {
		y--
	}
	era := y
	if y < 0 {
		era = y - 399
	}
	era /= 400
	yoe := y - era*400
	mp := m + 9
	if m > 2 {
		mp = m - 3
	}
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146_097 + doe - 719_468
}

// CivilFromDays is the inverse of DaysFromCivil: days-since-epoch → (y, m, d).
func CivilFromDays(z int64) (int64, int64, int64) {
	z += 719_468
	era := z
	if z < 0 {
		era = z - 146_096
	}
	era /= 146_097
	doe := z - era*146_097
	yoe := (doe - doe/1460 + doe/36_524 - doe/146_096) / 365
	y := yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d := doy - (153*mp+2)/5 + 1
	m := mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}

// FmtLinodeTS formats epoch seconds as the Linode-API timestamp
// (YYYY-MM-DDTHH:MM:SS, UTC, no offset).
func FmtLinodeTS(unix int64) string {
	day := floorDiv(unix, DaySecs)
	secs := floorMod(unix, DaySecs)
	h := secs / 3600
	mi := (secs % 3600) / 60
	s := secs % 60
	y, mo, d := CivilFromDays(day)
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d", y, mo, d, h, mi, s)
}

// ParseTS parses `YYYY-MM-DDTHH:MM:SS[.fff][Z]` (always UTC) to epoch seconds.
// The bool is false for empty or unparseable values.
func ParseTS(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	s = strings.TrimSuffix(s, "Z")
	date, timePart, ok := strings.Cut(s, "T")
	if !ok {
		return 0, false
	}
	dp := strings.Split(date, "-")
	if len(dp) != 3 {
		return 0, false
	}
	y, err1 := strconv.ParseInt(dp[0], 10, 64)
	mo, err2 := strconv.ParseInt(dp[1], 10, 64)
	d, err3 := strconv.ParseInt(dp[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	timePart, _, _ = strings.Cut(timePart, ".") // drop fractional seconds if present
	tp := strings.Split(timePart, ":")
	if len(tp) < 2 {
		return 0, false
	}
	h, err4 := strconv.ParseInt(tp[0], 10, 64)
	mi, err5 := strconv.ParseInt(tp[1], 10, 64)
	if err4 != nil || err5 != nil {
		return 0, false
	}
	var se int64
	if len(tp) >= 3 {
		var err6 error
		se, err6 = strconv.ParseInt(tp[2], 10, 64)
		if err6 != nil {
			return 0, false
		}
	}
	return DaysFromCivil(y, mo, d)*DaySecs + h*3600 + mi*60 + se, true
}

// floorDiv / floorMod implement Euclidean division (matching Rust's
// div_euclid / rem_euclid) so pre-epoch timestamps format correctly.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func floorMod(a, b int64) int64 {
	m := a % b
	if m != 0 && (a < 0) != (b < 0) {
		m += b
	}
	return m
}
