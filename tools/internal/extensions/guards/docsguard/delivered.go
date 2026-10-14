package docsguard

// delivered.go — which docs an instance actually carries.
//
// THE SET AND ITS CHECKER MUST BE THE SAME SET. `llz ci deliver-docs` prunes a
// copied-in docs/ to this list; `llz ci docs-guard` then validates every relative
// link AS IT WILL RESOLVE inside that pruned tree, because a link that works in
// the template and breaks after delivery is exactly the class of rot the guard
// exists to catch — and it is invisible from the template side. Two copies of the
// list would let the guard check a tree the deliverer does not ship, and the
// disagreement would look like a pass.
//
// It lives here rather than beside the deliverer because the guard is what makes
// the claim CHECKABLE, and the deliverer imports it to act on. That direction is
// deliberate: an authority with one consumer is a constant, an authority with two
// is a package.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"

// pathExists asks THROUGH THE FENCE. It took a joined absolute path and called
// os.Stat; a link target that climbed out of the tree ("../../etc/passwd") would
// therefore be reported as existing, and the guard would call the link valid on
// the strength of a file no instance carries.
func pathExists(repo capability.Repo, p string) bool {
	_, err := repo.Stat(p)
	return err == nil
}
