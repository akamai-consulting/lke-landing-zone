package provider_test

// Why this file exists, and why it is the ONLY test in package provider.
//
// provider.go declares an interface and nothing else — no implementation, no
// branches, no state. There is no behaviour to unit-test, and a test asserting
// that an interface has the methods it visibly has would be ceremony: it would
// raise the package's coverage number without raising anyone's confidence.
//
// What IS worth enforcing is the invariant the package exists to create. ADR 0013
// (docs/adr/0013-llz-as-apl-cli.md) establishes "two altitudes": the APL layer
// (internal/shared/apl/...) must reach provisioning ONLY through ClusterProvider,
// never by importing a concrete cloud, so a future non-Linode provider drops in
// without touching APL-layer code. That is an architectural claim, and today it
// holds only by convention — nothing fails if someone adds an import of
// internal/shared/linode to an APL-layer file.
//
// Mutation testing cannot find this class of gap: there is no operator to mutate.
// Coverage cannot either. It needs an explicit assertion about the build graph,
// which is what this is.
//
// The check is TRANSITIVE deliberately. A direct-import rule is easy to satisfy
// while still linking the concrete cloud in via an intermediary, and the point of
// the boundary is that the APL layer does not DEPEND on Linode, not merely that it
// does not name it.

import (
	"os/exec"
	"strings"
	"testing"
)

const modulePath = "github.com/akamai-consulting/lke-landing-zone/tools"

// forbidden are packages the APL layer must not depend on, transitively.
var forbidden = map[string]string{
	modulePath + "/internal/shared/linode": "the concrete cloud — reach it through provider.ClusterProvider instead (ADR 0013)",
	modulePath + "/cmd/llz":                "the CLI layer — the APL layer is a library and must not depend upward",
}

// deps returns every package pattern's transitive dependency set, via the build
// graph rather than a source scan, so an intermediary cannot launder the import.
func deps(t *testing.T, pattern string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pattern).Output()
	if err != nil {
		// `go` is always present where `go test` runs, so this is a real failure
		// rather than a reason to skip — skipping would make the guard vanish
		// silently, which is the failure mode this whole file exists to avoid.
		t.Fatalf("go list -deps %s: %v", pattern, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func TestAPLLayerDoesNotDependOnAConcreteCloud(t *testing.T) {
	for _, dep := range deps(t, "./../apl/...") {
		if why, bad := forbidden[dep]; bad {
			t.Errorf("internal/apl/... transitively depends on %s: %s", dep, why)
		}
	}
}

// The seam is only a boundary if the substrate side does not depend back on the
// layer it serves. A provider that imported the APL layer would make the two
// altitudes one.
//
// THE PREFIX NAMED A PATH THAT DOES NOT EXIST. It read `/internal/apl`, and the
// tree re-filed that package as `internal/shared/apl` — so the HasPrefix matched
// nothing and this test could not have failed. It is the same defect the sibling
// boundary_test.go carried in its `forbidden` map, found the same way and in the
// same pass: a layering rule keyed on a hardcoded package path is dead the moment
// the package moves, and dead in the direction that reports success.
func TestProviderDoesNotDependOnTheAPLLayer(t *testing.T) {
	const aplLayer = modulePath + "/internal/shared/apl"
	for _, dep := range deps(t, ".") {
		if strings.HasPrefix(dep, aplLayer) {
			t.Errorf("internal/provider depends on %s — the substrate must not depend on the layer above it", dep)
		}
	}
}

// A guard that cannot fail is worthless, and this one is unusual in that its
// subject is a build graph rather than a value — so prove the predicate
// discriminates by running it against a package that DOES legitimately depend on
// the concrete cloud. cmd/llz is the intended home for Linode day-0 code, so it
// must trip the same rule the APL layer must not.
//
// SKIPPING SELF IS WHAT MAKES IT PROVE ANYTHING. `go list -deps X` includes X, and
// `cmd/llz` is itself in `forbidden` — so without the skip this trips on the
// package being its own dependency and passes with every real edge deleted.
func TestForbiddenRuleActuallyDiscriminates(t *testing.T) {
	const self = modulePath + "/cmd/llz"
	const cloud = modulePath + "/internal/shared/linode"
	var tripped bool
	for _, dep := range deps(t, "./../../../cmd/llz") {
		if dep == self {
			continue // the package is its own dependency; that is not evidence
		}
		if dep == cloud {
			tripped = true
		}
	}
	if !tripped {
		t.Fatalf("cmd/llz does not transitively reach %s, so the rule above proves nothing — "+
			"either the Linode day-0 code moved, or `forbidden` no longer names anything real", cloud)
	}
}
