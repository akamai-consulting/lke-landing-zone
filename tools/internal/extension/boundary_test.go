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

var forbidden = map[string]string{
	modulePath + "/cmd/llz": "the CLI layer — this package is a library and must not depend upward (ADR 0014)",
	modulePath + "/internal/linode": "the concrete cloud — an extension declaration is cloud-agnostic; " +
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
// it. cmd/llz is where the Linode day-0 code lives and is itself the CLI layer.
func TestBoundaryRuleActuallyDiscriminates(t *testing.T) {
	var tripped bool
	for _, dep := range deps(t, "./../../cmd/llz") {
		if _, bad := forbidden[dep]; bad {
			tripped = true
		}
	}
	if !tripped {
		t.Fatal("cmd/llz trips no forbidden package, so the rule above proves nothing — " +
			"either the layering moved or `forbidden` no longer names anything real")
	}
}

// The declaration model is meant to be dependency-free: it describes shapes, it
// does not do I/O. If it ever needs yaml, a cluster client or the filesystem,
// that is a design change worth noticing rather than a stdlib import away.
func TestDeclarationModelStaysDependencyFree(t *testing.T) {
	const self = modulePath + "/internal/extension"
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
