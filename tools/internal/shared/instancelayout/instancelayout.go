package instancelayout

// Package instancelayout answers one question: where does this checkout keep its
// terraform roots and apl-values, and what are the tfvars paths under them.
//
// IT WAS EXTRACTED TO BREAK A CYCLE, not because a caller count crossed a
// threshold. `scaffold-instance` (closure 38), `env-topology` (21) and
// `config-readiness` (18) all reach into scaffold.go for these symbols, and
// scaffold.go reaches back into readiness.go and topology.go for theirs — so no
// ordering of those three extractions made any of them cheap. The four functions
// below were the whole shared surface, and `Detect` alone had FOURTEEN non-test
// callers.
//
// The distinction it encodes is small and load-bearing: a TEMPLATE-REPO checkout
// keeps the roots under instance-template/, a RENDERED INSTANCE keeps them at the
// repo root. Every command that touches a tfvars path has to agree on that, and
// before this package they agreed by each calling the same unexported function in
// a file about scaffolding.

import (
	"os"
	"path/filepath"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"
)

// Detect detects where the TF roots + overlays live and returns the
// terraform-iac-bootstrap root, the apl-values root, and the prefix to show in
// operator-facing paths. A template-repo checkout keeps them under
// instance-template/; a rendered instance keeps them at the repo root.
func Detect() (tfDir, aplDir, relPrefix string) {
	if fi, err := os.Stat(filepath.Join("instance-template", "terraform-iac-bootstrap")); err == nil && fi.IsDir() {
		return filepath.Join("instance-template", "terraform-iac-bootstrap"),
			filepath.Join("instance-template", "apl-values"), "instance-template/"
	}
	return "terraform-iac-bootstrap", "apl-values", ""
}

var Roots = []string{"cluster", "object-storage", "databases"}

// optionalRoots are roots an instance may legitimately never apply. `llz render`
// still writes a tfvars stub for them (render is per-root, not per-opt-in), but a
// MISSING one is not a defect — so readiness must not report it as such.
//
// This matters for legacy (pre-spec) instances: they never run `llz render`, their
// hand-authored <env>.tfvars are the tracked source of truth, and readiness does
// flag a genuinely missing one for them. Without this set, adding `databases` to
// Roots would have every such instance reporting a blocking "missing
// databases/<env>.tfvars — run llz env add" for a database they never asked for.
var optionalRoots = map[string]bool{"databases": true}

// Optional reports whether path is a tfvars belonging to an optional root.
func Optional(path string) bool {
	return optionalRoots[filepath.Base(filepath.Dir(path))]
}

// ValidateOBJCluster catches a value that isn't shaped like a Linode OBJ cluster
// id. The shape rule lives in internal/validate (OBJClusterID) so the LandingZone
// spec validator reuses it.
func ValidateOBJCluster(v string) error { return validate.OBJClusterID(v) }

func TFVarsPaths(tfDir, env string) []string {
	var out []string
	for _, root := range Roots {
		out = append(out, filepath.Join(tfDir, root, env+".tfvars"))
	}
	return out
}
