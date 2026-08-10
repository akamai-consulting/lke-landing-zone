package openbao

// quote.go — a local copy of ghcli.Quote, and the ONE copy in this campaign made
// to break an import cycle rather than to avoid ceremony.
//
// Importing internal/ghcli from here is a cycle five hops long:
//
//	openbao -> ghcli -> configreadiness -> tokeninv -> reconcilelanes -> openbao
//
// and the reason is one dry-run line that prints a kubectl command for a human to
// paste. Twelve lines of shell quoting is a much smaller price than any of the
// four other edges in that chain, none of which is wrong.
//
// SINGLE quotes, same as ghcli.Quote and for the same reason: double quotes would
// leave $VAR and backticks expandable in a value we are only trying to display —
// and this particular line prints "$OPENBAO_ROOT_TOKEN" verbatim ON PURPOSE, so a
// double-quoted rendering would be actively misleading.
//
// If this ever drifts from ghcli.Quote, prefer ghcli's: it is the one with tests
// covering the escape cases.

import "strings"

func quote(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		if a == "" || strings.ContainsAny(a, " \t\"'$&|;<>()") {
			b.WriteString("'" + strings.ReplaceAll(a, "'", `'\''`) + "'")
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}

// firstNonEmpty is copied, not shared. Twelfth package in this campaign to keep
// its own three lines.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
