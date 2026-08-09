package main

// orQ is package main's copy of the two-line helper internal/configreadiness also
// has: render "?" when a value could not be determined, so an UNKNOWN is never
// printed as an empty string. Copied rather than imported — the reap verbs would
// otherwise depend on the readiness extension to format one cell.

// orQ renders a value, or "?" when it's the unknown/zero case (display only).
func orQ(s string, unknown bool) string {
	if unknown {
		return "?"
	}
	return s
}
