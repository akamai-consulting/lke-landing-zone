package registry

// registration_test.go — a package that DECLARES an extension must be REGISTERED.
//
// ────────────────────────────────────────────────────────────────────────────
// THE ONE DIRECTION NOTHING CHECKED.
//
// package_test.go walks registry -> filesystem: every extension in All() must
// resolve to a directory that exists and holds Go source. That catches a
// constructor that moved.
//
// Nothing walked filesystem -> registry. `declarations` in registry.go is a
// hand-maintained import list, and its own header calls forgetting an entry
// "loud (the extension is simply absent from `llz extension list`)". It is not
// loud, it is silent, and the consequences are the ones this tree refuses
// everywhere else:
//
//   - a GATE stops being driven, and `llz ci gates` still prints `N ran, all
//     clean` — the vacuous-green shape undrivenGates was converted from prose to
//     data to prevent, arriving one level up.
//   - an ASSERT LANE stops being resolvable for enablement, and
//     laneDisabledForInstance answers "run it" for a name the registry does not
//     know, which is the right answer to the wrong question.
//   - Validate() never sees the declaration, so the three ceiling tables do not
//     apply to it. An extension nobody registered is an extension nobody linted.
//
// The cost of the miss is highest for exactly the extension most likely to be
// missed: a NEW one, whose author has written a declaration, a binding and a
// command, and has one import line left to remember.
//
// TWO GUARDS IN THIS PACKAGE ALREADY WALK THIS TREE — grants_back_capabilities
// and commands_census — so the walk is not the work. Nobody had pointed one at
// the registry itself.
// ────────────────────────────────────────────────────────────────────────────

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// declaresAnExtension reports whether a directory holds a constructor returning a
// declaration, ignoring comment lines.
//
// COMMENTS ARE STRIPPED for the reason every scanner here gives: the
// headers here narrate the model constantly and several of them spell
// `extension.Extension{` while describing it. Matching prose would report a
// package registered because it talked about being one.
func declaresAnExtension(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("reading %s: %v", n, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, "extension.Extension{") {
				return true
			}
		}
	}
	return false
}

func TestEveryDeclaringPackageIsRegistered(t *testing.T) {
	root := extensionsDir(t)

	// What the registry knows, as paths relative to internal/extensions — the same
	// spelling Package() returns.
	registered := map[string]bool{}
	for _, e := range All() {
		if pkg, ok := Package(e.Name); ok {
			registered[pkg] = true
		}
	}

	var unregistered []string
	var declaring int
	for _, dir := range packageDirs(t, root) {
		if !declaresAnExtension(t, dir) {
			continue
		}
		declaring++
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatalf("relativising %s: %v", dir, err)
		}
		if pkg := filepath.ToSlash(rel); !registered[pkg] {
			unregistered = append(unregistered, pkg)
		}
	}

	// A walk that found nothing would report every package registered.
	if declaring == 0 {
		t.Fatal("no package under internal/extensions declares an extension — the detection has " +
			"drifted from the code and this guard would pass however many were missing")
	}

	sort.Strings(unregistered)
	if len(unregistered) > 0 {
		t.Errorf("%d package(s) declare an extension that registry.go does not import:\n\t%s\n"+
			"\tAdd the import and the `declarations` entry. Until then the extension is absent "+
			"from `llz extension list`, its gate is not driven while the suite still reports "+
			"`all clean`, and Validate() never lints its declaration.",
			len(unregistered), strings.Join(unregistered, "\n\t"))
	}
	t.Logf("%d declaring packages, %d registered extensions", declaring, len(All()))
}

// THE DETECTOR MUST DISCRIMINATE, because the test above passes on a clean tree by
// construction and would pass just as quietly if `declaresAnExtension` stopped
// recognising a declaration — at which point `declaring` counts every package and
// the guard checks nothing about the ones that matter.
func TestTheDeclarationDetectorDiscriminates(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("helper.go", "package x\n\nfunc f() int { return 1 }\n")
	if declaresAnExtension(t, dir) {
		t.Error("a package with no declaration was reported as declaring one")
	}

	// Prose about the model is not a declaration — this is the exact shape that
	// would have made the count wrong.
	write("doc.go", "package x\n\n// Every extension returns an extension.Extension{} from Extension().\n")
	if declaresAnExtension(t, dir) {
		t.Error("a COMMENT mentioning extension.Extension{} was read as a declaration — the " +
			"comment strip is what keeps the population honest")
	}

	// A test file is not a declaration either: a fixture must not register a
	// package that ships nothing.
	write("thing_test.go", "package x\n\nvar e = extension.Extension{Name: \"fixture\"}\n")
	if declaresAnExtension(t, dir) {
		t.Error("a _test.go fixture was read as a declaration")
	}

	write("extension.go", "package x\n\nfunc Extension() extension.Extension {\n\treturn extension.Extension{Name: \"real\"}\n}\n")
	if !declaresAnExtension(t, dir) {
		t.Error("a real declaration was not detected — this guard has been vacuous since it stopped")
	}
}
