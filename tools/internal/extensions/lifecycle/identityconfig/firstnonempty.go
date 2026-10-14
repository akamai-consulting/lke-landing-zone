package identityconfig

// firstNonEmpty is copied, not shared: three lines are cheaper than a shared
// package every caller would have to import.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
