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

// refusalMaxLines bounds RefusalText so a pathological message cannot bury a
// report, while still being long enough for the shapes that matter: a Kyverno
// policy body runs to about six lines, a multi-error apiserver validation to about
// a dozen.
const refusalMaxLines = 14

// RefusalText renders a whole apiserver refusal for a report line, indented so it
// reads as one quoted block.
//
// FirstLine IS WRONG FOR A REFUSAL, and quietly so. A denial's REASON is never on
// line one: kubectl prints `admission webhook "…" denied the request:` and the
// policy, the rule and the actual message on the lines below. So an arm that
// exists to "say what happened and let the reader judge" printed the header and
// discarded everything worth judging — the same state that arm's own comment
// records an earlier version being in, one layer down. FirstLine stays as it is
// for the callers that want a summary; this is for the ones that owe the reader
// the evidence.
func RefusalText(msg string) string {
	var kept []string
	for _, ln := range strings.Split(msg, "\n") {
		ln = strings.TrimRight(strings.ReplaceAll(ln, "\r", ""), " \t")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if len(ln) > firstLineMax {
			ln = ln[:firstLineMax] + "…"
		}
		kept = append(kept, ln)
		if len(kept) == refusalMaxLines {
			kept = append(kept, "… (truncated)")
			break
		}
	}
	if len(kept) == 0 {
		return "(the apiserver said nothing)"
	}
	if len(kept) == 1 {
		return kept[0]
	}
	return "\n      " + strings.Join(kept, "\n      ")
}
