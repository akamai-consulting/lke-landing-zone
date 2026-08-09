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

// PinnedTemplateRef reports the template ref this instance was rendered from.
//
// INJECTED, NOT MOVED, and it is the only edge in this set that could not be cut
// cheaply. Its implementation reads the copier answers file through `readAnswers`,
// which lives inside the scaffold mass that this campaign has recorded as blocked
// — it is the CLI's own front end, woven through main.go's globals, and chasing it
// here would drag that whole knot in for one string.
//
// The default returns "" rather than guessing. A wrong ref would be bootstrapped
// into a real cluster; an empty one makes the caller say so.
var PinnedTemplateRef = func() string { return "" }
