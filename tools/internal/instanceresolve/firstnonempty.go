package instanceresolve

// firstNonEmpty is copied, not shared. Fourteenth package in this campaign to
// keep its own three lines.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
