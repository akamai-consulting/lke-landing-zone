package extension_test

// The declaration model is a library. Its whole reason for existing is that the
// extension framework must NOT accrete into package main (ADR 0014) — so the one
// thing that would quietly undo it is this package growing a dependency back on
// cmd/llz, or on a concrete cloud, the moment someone wires the first real
// extension up.
//
// Nothing else would notice. Coverage cannot see it; the core-surface budget
// counts package main and would happily watch the boundary dissolve from the
// other side. So, as internal/provider does for ADR 0013's two altitudes, the
// claim is asserted against the build graph — transitively, because an
// intermediary would otherwise launder the import.

import (
	"os/exec"
	"strings"
	"testing"
)

const modulePath = "github.com/akamai-consulting/lke-landing-zone/tools"

// forbidden is what this package must not reach.
//
// `internal/cli` IS THE ROW THAT WAS MISSING, and its absence made the rule name a
// package that had stopped being the thing it was guarding. This map held
// `cmd/llz` alone, described as "the CLI layer" — true when it was written, and
// false after ADR 0014's amendment moved the tree: `cmd/llz` is now a six-logic-line
// entry point whose only import is `internal/cli`, so the CLI layer this package
// must not depend upward on was not listed at all.
//
// Both are listed now. `cmd/llz` stays because depending on an entry point is its
// own kind of wrong, and because deleting a row to replace it would lose the reason
// it was there.
//
// AND THE ADR 0013 ROW NAMED A PACKAGE THAT DOES NOT EXIST. It read
// `internal/linode`; the tree re-filed that as `internal/shared/linode`, and a
// forbidden entry matching nothing forbids nothing — this package could have
// imported the concrete cloud and passed. The strengthened discriminator below is
// what found it, on its first run, which is the whole argument for a discriminator
// that has to locate a real edge rather than one satisfied by a package being its
// own dependency.
var forbidden = map[string]string{
	modulePath + "/internal/cli": "the CLI layer — this package is a library and must not depend upward (ADR 0014)",
	modulePath + "/cmd/llz":      "the entry point — same direction, one level further up (ADR 0014)",
	modulePath + "/internal/shared/linode": "the concrete cloud — an extension declaration is cloud-agnostic; " +
		"reach provisioning through provider.ClusterProvider (ADR 0013)",
}

func deps(t *testing.T, pattern string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pattern).Output()
	if err != nil {
		// `go` is always present where `go test` runs, so this is a real failure
		// rather than a reason to skip — a skip would make the guard vanish
		// silently, which is the failure mode it exists to prevent.
		t.Fatalf("go list -deps %s: %v", pattern, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func TestExtensionModelDoesNotDependUpwardOrOnACloud(t *testing.T) {
	for _, dep := range deps(t, ".") {
		if why, bad := forbidden[dep]; bad {
			t.Errorf("internal/extension transitively depends on %s: %s", dep, why)
		}
	}
}

// A guard whose subject is a build graph cannot be mutation-tested, so prove the
// predicate discriminates by running it against a package that legitimately trips
// it: cmd/llz reaches the CLI layer and, through it, the concrete cloud.
//
// IT USED TO PROVE THIS FOR FREE, WHICH IS TO SAY NOT AT ALL. `go list -deps X`
// includes X, so with `cmd/llz` in the map the old version tripped on the package
// being ITS OWN dependency — true whatever the layering does, and it would have
// stayed true if every real edge had been deleted. A discriminator satisfied by a
// tautology is the vacuous-green shape this tree refuses everywhere; the fix is to
// require an edge the walk had to FIND.
func TestBoundaryRuleActuallyDiscriminates(t *testing.T) {
	const self = modulePath + "/cmd/llz"
	tripped := map[string]bool{}
	for _, dep := range deps(t, "./../../../cmd/llz") {
		if dep == self {
			continue // the package is its own dependency; that is not evidence
		}
		if _, bad := forbidden[dep]; bad {
			tripped[dep] = true
		}
	}
	for _, want := range []string{modulePath + "/internal/cli", modulePath + "/internal/shared/linode"} {
		if !tripped[want] {
			t.Errorf("cmd/llz does not transitively reach %s, so `forbidden` naming it proves "+
				"nothing about this package staying clear of it — either the layering moved or "+
				"the row no longer names anything real", want)
		}
	}
}

// The declaration model is meant to be dependency-free: it describes shapes, it
// does not do I/O. If it ever needs yaml, a cluster client or the filesystem,
// that is a design change worth noticing rather than a stdlib import away.
func TestDeclarationModelStaysDependencyFree(t *testing.T) {
	const self = modulePath + "/internal/shared/extension"
	for _, dep := range deps(t, ".") {
		if dep == self {
			continue // `go list -deps` includes the package itself
		}
		if strings.HasPrefix(dep, modulePath+"/") {
			t.Errorf("internal/extension depends on in-module package %s — "+
				"the declaration model should describe shapes, not reach for behaviour", dep)
		}
		// A dotted FIRST path element is a module host; stdlib paths never have one.
		if host := strings.SplitN(dep, "/", 2)[0]; strings.Contains(host, ".") {
			t.Errorf("internal/extension depends on third-party %s — keep the declaration model stdlib-only", dep)
		}
	}
}
