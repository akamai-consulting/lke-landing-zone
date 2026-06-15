package health

// sla.go is the pure decision logic behind the scheduled credential SLA checks
// in .github/workflows/llz-scheduled-checks.yml — the age-vs-threshold rotation
// and GitHub service-PAT header classifications the jobs used to do inline with
// `date -d` and `curl | grep`. Keeping it here (side-effect free, like the rest
// of internal/health) makes every threshold edge the shell silently got wrong —
// off-by-one days, missing-header vs unparseable, already-expired vs over-policy
// — a unit test. cmd/llz wires kubectl / HTTP to these.

import (
	"strings"
	"time"
)

// secondsPerDay matches the shell's `(now - then) / 86400` integer-day math.
const secondsPerDay = 24 * time.Hour

// DaysSince returns the whole days between then and now (floored toward zero,
// like the shell division). Negative when then is in the future.
func DaysSince(then, now time.Time) int {
	return int(now.Sub(then) / secondsPerDay)
}

// DaysUntil returns the whole days from now until t (floored). Negative once t
// has passed — the "already expired" signal for the expiry checks.
func DaysUntil(t, now time.Time) int {
	return int(t.Sub(now) / secondsPerDay)
}

// MaxTime returns the latest time in ts (ok=false when ts is empty) — the
// "newest credential wins" reduction the rotation-SLA checks apply to a set of
// timestamps (lke-admin-token Secrets' creationTimestamps, an Argo
// CronWorkflow's successful-run finishedAts) before measuring age.
func MaxTime(ts []time.Time) (time.Time, bool) {
	if len(ts) == 0 {
		return time.Time{}, false
	}
	max := ts[0]
	for _, t := range ts[1:] {
		if t.After(max) {
			max = t
		}
	}
	return max, true
}

// ClassifyRotationAge classifies a credential's age (in whole days) against a
// warn and an optional critical threshold — the shared verdict behind the
// approle / lke-admin / loki-objkey rotation-SLA jobs. A criticalDays <= 0
// means "no critical tier" (warn-only, e.g. approle's single 100-day window).
//
//	age >= critical (when set) -> CatFail   (::error::, fails the job)
//	age >= warn                -> CatWarn   (::warning::, non-failing)
//	otherwise                  -> CatOK
func ClassifyRotationAge(ageDays, warnDays, criticalDays int) Category {
	switch {
	case criticalDays > 0 && ageDays >= criticalDays:
		return CatFail
	case ageDays >= warnDays:
		return CatWarn
	default:
		return CatOK
	}
}

// expiryLayouts are the timestamp forms the GitHub
// `GitHub-Authentication-Token-Expiration` header (and `date -d`-friendly
// inputs) appear in, tried in order. The header is documented as
// "2006-01-02 15:04:05 -0700" but UTC/zone-name and RFC3339 variants show up too.
var expiryLayouts = []string{
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05 UTC",
	time.RFC3339,
	time.RFC1123Z,
	"2006-01-02",
}

// ParseExpiryTime parses an expiry timestamp using the known layouts, returning
// ok=false when none match (the "unparseable expiry — verify manually" branch).
func ParseExpiryTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	// "UTC" isn't a numeric zone Go's MST layout accepts, so normalize it.
	norm := strings.Replace(s, " UTC", " +0000", 1)
	for _, layout := range expiryLayouts {
		for _, cand := range []string{s, norm} {
			if t, err := time.Parse(layout, cand); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// PATCheckState is the outcome of one GitHub service-PAT expiry self-check.
// GitHub exposes no classic-PAT list API, so each token is probed individually
// and its `GitHub-Authentication-Token-Expiration` response header inspected.
type PATCheckState int

const (
	PATNotSet      PATCheckState = iota // token not provided — warn, non-failing
	PATUnreachable                      // API unreachable (HTTP code 0) — warn, non-failing
	PATInvalid                          // 401/403 — token invalid/revoked/expired — fail
	PATNoExpiry                         // no expiration header — never-expiring classic PAT — fail
	PATUnparseable                      // expiry header present but unparseable — warn
	PATExpired                          // expiry has passed — fail
	PATOverPolicy                       // lifetime exceeds the max-days policy — fail
	PATWarn                             // within the warn window — warn
	PATOK                               // healthy — expiry set and within policy
)

// Category maps a PAT check state to the report category that drives the
// annotation level and the pass/fail gate: only the four hard states fail.
func (s PATCheckState) Category() Category {
	switch s {
	case PATInvalid, PATNoExpiry, PATExpired, PATOverPolicy:
		return CatFail
	case PATNotSet, PATUnreachable, PATUnparseable, PATWarn:
		return CatWarn
	default:
		return CatOK
	}
}

// ClassifyPATResponse classifies one service-PAT self-check from its raw inputs:
// whether the token was provided, the HTTP status (0 == unreachable), and the
// raw expiry header ("" when absent). daysLeft is meaningful only for the
// expiry-derived states (Expired/OverPolicy/Warn/OK). The ordering mirrors the
// job's `check()`: connectivity and auth first, then header presence, then the
// expired / over-policy / warn / ok ladder.
func ClassifyPATResponse(tokenPresent bool, httpCode int, expHeader string, now time.Time, maxDays, warnDays int) (state PATCheckState, daysLeft int) {
	switch {
	case !tokenPresent:
		return PATNotSet, 0
	case httpCode == 0:
		return PATUnreachable, 0
	case httpCode == 401 || httpCode == 403:
		return PATInvalid, 0
	}
	if strings.TrimSpace(expHeader) == "" {
		return PATNoExpiry, 0
	}
	exp, ok := ParseExpiryTime(expHeader)
	if !ok {
		return PATUnparseable, 0
	}
	daysLeft = DaysUntil(exp, now)
	switch {
	case daysLeft <= 0:
		return PATExpired, daysLeft
	case daysLeft > maxDays:
		return PATOverPolicy, daysLeft
	case daysLeft <= warnDays:
		return PATWarn, daysLeft
	default:
		return PATOK, daysLeft
	}
}
