package chartguard

// deps.go — what the chart guards are handed.
//
// The smallest Deps in the repo, and that is the finding rather than an accident:
// a gate reaches nothing, so the only capability it needs is the one thing it
// cannot do from a file read — ask git what changed.

// Deps carries what this package cannot reach for itself.
type Deps struct {
	// GitOutput runs git in dir and returns trimmed stdout.
	//
	// THE VERSION GUARD IS ABOUT A DIFF, not about a tree, which is why it needs
	// this and the other two guards do not. `chart-version-guard` fires on any
	// edit inside a chart directory because publish-charts only ever pushes a NEW
	// version — so the question it asks is "did this commit touch a chart without
	// moving its Chart.yaml", and only git can answer it.
	GitOutput func(dir string, args ...string) (string, error)
}
