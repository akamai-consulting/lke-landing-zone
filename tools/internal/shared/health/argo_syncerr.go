package health

// argo_syncerr.go — rendering a retrying-sync message into one report line.

import "strings"

// firstLineMax bounds the excerpt. Argo's sync messages carry a whole Kyverno
// policy report — many lines, most of it YAML — and a report line is read at a
// glance. The full text is in the Application status the diagnostics dump.
const firstLineMax = 220

// FirstLine reduces a multi-line operation message to the sentence worth putting
// on a report line: leading blanks dropped, collapsed to one line, bounded.
//
// The useful part of Argo's retrying-sync message is its FIRST sentence — "one or
// more synchronization tasks completed unsuccessfully, reason: admission webhook
// … denied the request" — and everything after it is the policy report that
// belongs in the diagnostics group, not in a census line.
func FirstLine(msg string) string {
	for _, ln := range strings.Split(msg, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if len(ln) > firstLineMax {
			return ln[:firstLineMax] + "…"
		}
		return ln
	}
	return strings.TrimSpace(msg)
}
