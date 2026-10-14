package extension_test

// internal/shared is substrate and internal/extensions is capability. The whole
// point of the split is that the arrow only points one way — an extension may
// reach down for a shared helper, and a shared helper must never reach up for an
// extension.
//
// IT HAD ALREADY DRIFTED FOUR TIMES before anything checked. Each one arrived the
// same way and each one looked reasonable in isolation: some genuinely general
// helper — parsing a version, reading a token out of an env var or a file, reading
// a stamp file — got written inside whichever extension needed it FIRST, and then
// the second caller, down in shared, imported the extension rather than moving the
// helper. Nobody chose to invert the layering; they chose not to move a function.
//
// The damage is not aesthetic. A shared package that imports an extension drags
// that extension's whole dependency tree into everything downstream of it, and it
// makes the extension impossible to delete or reshape without breaking substrate
// that has nothing to do with it. It also quietly defeats the in-degree property
// the extension set is supposed to have.
//
// SO THE RULE IS CHECKED AGAINST THE BUILD GRAPH, on DIRECT non-test imports.
// Direct is the right granularity: a transitive inversion has a direct one
// underneath it somewhere, and that is the package worth naming in the failure.
// Non-test matters too — see the exemptions below.

import (
	"os/exec"
	"strings"
	"testing"
)

const (
	sharedPrefix    = modulePath + "/internal/shared/"
	extensionPrefix = modulePath + "/internal/extensions/"
	verbPrefix      = modulePath + "/internal/verbs/"
	registryPkg     = modulePath + "/internal/shared/extension/registry"
)

// upward names the trees internal/shared must never depend on: extensions are
// capabilities, verbs are the CLI commands that compose them. Both sit ABOVE
// shared. `verbs` was added when the composite commands (onboard, lint, upgrade)
// moved out of internal/extensions -- without it the rule would have had a hole
// exactly where the most-composed code went, which is where it is most tempting to
// reach back down and then sideways.
var upward = []string{extensionPrefix, verbPrefix}

// imports returns each package matching pattern with its DIRECT, non-test
// imports. Test imports are deliberately outside the rule: a shared package's
// external (_test) test may use a real extension as a fixture — clusterspec's
// tests use envdef and render precisely because a hand-rolled fake would prove
// less — and Go already rejects the in-package cycle, so the escape hatch cannot
// be abused into a production dependency.
func imports(t *testing.T, pattern string) map[string][]string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}} {{join .Imports \" \"}}", pattern).Output()
	if err != nil {
		t.Fatalf("go list %s: %v", pattern, err)
	}
	m := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			m[f[0]] = f[1:]
		}
	}
	return m
}

func TestSharedPackagesDoNotImportExtensions(t *testing.T) {
	for pkg, deps := range imports(t, "./../...") {
		// THE REGISTRY IS EXEMPT, and has to be. Its entire job is to import all
		// ~75 extensions so that main can hold a table instead of 75 imports; it
		// is the one place the arrow is supposed to point up. Exempting it is not
		// a weakening of the rule — a guard that failed on day one for a reason
		// everyone already knew would simply have been deleted.
		if pkg == registryPkg {
			continue
		}
		for _, dep := range deps {
			for _, up := range upward {
				if !strings.HasPrefix(dep, up) {
					continue
				}
				t.Errorf("%s imports %s\n\tinternal/shared must not depend on %s. "+
					"Either move the symbol down into shared (if it is substrate — most are), "+
					"or pass the answer in as a parameter (if it encodes a caller's policy).",
					strings.TrimPrefix(pkg, sharedPrefix), strings.TrimPrefix(dep, up),
					strings.TrimSuffix(strings.TrimPrefix(up, modulePath+"/"), "/"))
			}
		}
		// The registry launders the exemption: anything importing it inherits a
		// dependency on every extension there is. Only cmd/llz should hold it.
		for _, dep := range deps {
			if dep == registryPkg {
				t.Errorf("%s imports the extension registry, which imports every extension — "+
					"that is the layering violation above, laundered through one hop",
					strings.TrimPrefix(pkg, sharedPrefix))
			}
		}
	}
}

// A build-graph guard cannot be mutation-tested, so prove the detector fires by
// pointing it at the one package that legitimately trips it. If the registry ever
// stops importing extensions, the loop above is scanning for something that no
// longer exists and the exemption is stale.
func TestLayeringRuleActuallyDiscriminates(t *testing.T) {
	var found bool
	for _, dep := range imports(t, registryPkg)[registryPkg] {
		if strings.HasPrefix(dep, extensionPrefix) {
			found = true
		}
	}
	if !found {
		t.Fatal("the extension registry imports no extension, so the rule above proves nothing — " +
			"either the registry moved or the extensions/ prefix is wrong")
	}
}
