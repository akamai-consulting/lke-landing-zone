package cli

// capability_wiring_test.go — the installers must hand out LIVE handles.
//
// ────────────────────────────────────────────────────────────────────────────
// THIS IS THE TEST THAT WOULD HAVE CAUGHT IT.
//
// Two assert-suite lanes shipped wired to a refusing Writer. Everything was green:
// the declarations validated, the registry listed them, `llz ci gates` passed, and
// the unit tests passed — because a refusing handle is non-nil, satisfies the
// interface, and is what a CORRECT assembly also produces for a grant the binding
// genuinely lacks. Nothing distinguished "this lane may not write" from "this lane
// was wired to the wrong binding".
//
// mustWriter/mustCluster make the second case panic at assembly. That only helps
// if assembly RUNS, and in a built binary it runs from init() — which a unit test
// suite never exercises for the paths it does not import. So this calls every
// installer by name.
//
// THE LIST IS DERIVED, NOT TRANSCRIBED. installers_test lists the functions, and
// TestEveryInstallerIsExercised compares that list against every `func
// install…Deps()` in the package source, so a tenth installer cannot be added
// without either being covered or the build going red. A hand-maintained list of
// what to test is the shape that let `Bindings[0]` sit in six files at once.
// ────────────────────────────────────────────────────────────────────────────

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// installers is every Deps assembly in this package, keyed by its declared name.
//
// A MAP RATHER THAN A SLICE OF FUNCTION VALUES, because the installers do not
// share a signature — installConvergeDeps takes globalOpts. Keying by name lets
// the derived check below compare against the source without reflection having to
// see through a wrapper, and it is the name a failure should print anyway.
var installers = map[string]func(){
	"installAssertNetworkDeps":    installAssertNetworkDeps,
	"installAssertPlatformDeps":   installAssertPlatformDeps,
	"installAssertIdentityDeps":   installAssertIdentityDeps,
	"installAssertReconcilerDeps": installAssertReconcilerDeps,
	"installAssertObsDeps":        installAssertObsDeps,
	"installAssertSecretsDeps":    installAssertSecretsDeps,
	"installConfigReadinessDeps":  installConfigReadinessDeps,
	"installDoctorDeps":           installDoctorDeps,
	"installEnvTopologyDeps":      installEnvTopologyDeps,
	// It takes no globalOpts any more, which is the point: no flag can change
	// which binding was selected, and the one flag it used to carry froze at its
	// pre-parse zero. See installConvergeDeps.
	"installConvergeDeps": installConvergeDeps,
}

// TestEveryInstalledCapabilityIsLive runs each installer. A Deps that selects a
// binding lacking the grant its handle needs panics inside mustWriter/mustCluster,
// and the panic message names the binding that was actually chosen.
func TestEveryInstalledCapabilityIsLive(t *testing.T) {
	names := make([]string, 0, len(installers))
	for n := range installers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		install := installers[name]
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s panicked while assembling its capabilities:\n\t%v\n"+
						"\tThe binding named in that message does not declare the grant the handle "+
						"needs, so the lane would have run with a present-but-refusing handle and "+
						"failed at its first mutation with a permission error.", name, r)
				}
			}()
			install()
		})
	}
}

// TestEveryInstallerIsExercised derives the population from the source, so the
// list above cannot fall behind the package.
func TestEveryInstallerIsExercised(t *testing.T) {
	declared := map[string]bool{}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", n), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", n, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			if strings.HasPrefix(fn.Name.Name, "install") && strings.HasSuffix(fn.Name.Name, "Deps") {
				declared[fn.Name.Name] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no install…Deps functions in this package — the naming convention changed " +
			"and this guard has been comparing an empty set since it did")
	}

	covered := map[string]bool{}
	for n := range installers {
		covered[n] = true
	}

	var missing, stale []string
	for n := range declared {
		if !covered[n] {
			missing = append(missing, n)
		}
	}
	for n := range covered {
		if !declared[n] {
			stale = append(stale, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)

	if len(missing) > 0 {
		t.Errorf("%d Deps installer(s) are never exercised:\n\t%s\n"+
			"\tAdd them to `installers`. An unexercised assembly is one whose capability wiring "+
			"nothing checks, which is how two lanes shipped unable to mutate.",
			len(missing), strings.Join(missing, "\n\t"))
	}
	if len(stale) > 0 {
		t.Errorf("`installers` names %d function(s) this package no longer declares:\n\t%s",
			len(stale), strings.Join(stale, "\n\t"))
	}
}
