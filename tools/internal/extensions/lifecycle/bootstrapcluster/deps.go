package bootstrapcluster

// deps.go — the two edges this package could not bring with it.

// firstNonEmpty is copied, not shared. Thirteenth package in this campaign to
// keep its own three lines.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
