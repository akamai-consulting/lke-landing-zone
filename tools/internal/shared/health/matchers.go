package health

import (
	"regexp"
	"strings"
)

// matchers.go ports the two name-matching helpers the script uses to route a
// resource to the pending/deferred channels: phase1_matches (literal name or
// name-<suffix>) and external_dep_reason (regex pattern, suffix-tolerant, → reason).

// MatchPrefix mirrors phase1_matches: needle matches when it equals an item or
// begins with item + "-" (a deployment-hash / generated suffix).
func MatchPrefix(needle string, items []string) bool {
	for _, item := range items {
		if needle == item || strings.HasPrefix(needle, item+"-") {
			return true
		}
	}
	return false
}

// DepEntry pairs a name pattern (a regex, as the bash `=~` used) with the
// human-readable reason printed when a resource matches it.
type DepEntry struct {
	Pattern string
	Reason  string
}

// MatchExternalDep mirrors external_dep_reason: needle matches an entry when it
// matches ^(<pattern>)(-.*)?$ — the pattern, optionally followed by a "-suffix"
// (covers generated ReplicaSet/Pod names). Returns the matching entry's reason.
// A malformed pattern is skipped (never panics) rather than matching nothing-loudly.
func MatchExternalDep(needle string, entries []DepEntry) (reason string, ok bool) {
	for _, e := range entries {
		re, err := regexp.Compile("^(" + e.Pattern + ")(-.*)?$")
		if err != nil {
			continue
		}
		if re.MatchString(needle) {
			return e.Reason, true
		}
	}
	return "", false
}
