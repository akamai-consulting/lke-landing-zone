package bootstrapcluster

// deps.go — the two edges this package could not bring with it.

// firstNonEmpty is copied, not shared. Three lines are cheaper than a
// shared package every caller would have to import.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
