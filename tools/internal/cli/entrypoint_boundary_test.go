package cli

// entrypoint_boundary_test.go — tools/cmd/llz may import this package and nothing
// else.
//
// ────────────────────────────────────────────────────────────────────────────
// THE BUDGET ALONE WOULD NOT HOLD THE LINE.
//
// cmd-llz-entrypoint pins that package to six logic lines, which stops it GROWING.
// It does not stop those six lines from being the wrong six: `import
// ".../internal/extensions/lifecycle/teardown"` and one AddCommand is well under
// budget and puts a command back in the package nothing can import.
//
// That is how the original condition arrived — not as a decision, but one
// reasonable-looking line at a time, each too small to argue with. The budget
// counts lines; this counts DIRECTIONS, and the direction is the property the move
// was for.
//
// IT LIVES IN internal/cli, NOT IN cmd/llz. A test inside cmd/llz would be a test
// in package main, which is the thing this whole change exists to stop needing. It
// reads the entry point's source rather than importing it, because importing
// package main is precisely what Go forbids.
// ────────────────────────────────────────────────────────────────────────────

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const entrypointDir = "../../cmd/llz"

func TestEntrypointImportsNothingButThisPackage(t *testing.T) {
	// Derived, so this keeps checking the real thing if the package moves again.
	full := runtime.FuncForPC(reflect.ValueOf(Main).Pointer()).Name()
	selfPath := full[:strings.LastIndex(full, ".")]

	entries, err := os.ReadDir(entrypointDir)
	if err != nil {
		t.Fatalf("the entry point must exist: %v", err)
	}

	var sources int
	var offenders []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		sources++
		f, err := parser.ParseFile(fset, filepath.Join(entrypointDir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == selfPath || isStdlib(path) {
				continue
			}
			offenders = append(offenders, name+" imports "+path)
		}
	}

	// A census that found nothing agrees with any rule. If the glob or the
	// directory moved, this must fail rather than pass silently.
	if sources == 0 {
		t.Fatalf("no non-test Go found under %s — this test would pass over an empty set", entrypointDir)
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("tools/cmd/llz reaches past %s:\n\t%s\n"+
			"\tThe entry point holds the `main` symbol and the os.Exit, and nothing else. "+
			"Anything it imports directly is wired up in the one package that cannot be "+
			"imported or tested from outside — which is the condition moving the tree to "+
			"internal/cli exists to end.",
			selfPath, strings.Join(offenders, "\n\t"))
	}
}

// isStdlib reports whether an import path is a standard-library package.
//
// THE FIRST SEGMENT HAVING NO DOT is the test the go tool itself uses: every module
// path outside the standard library begins with a hostname.
func isStdlib(path string) bool {
	first := path
	if i := strings.Index(path, "/"); i >= 0 {
		first = path[:i]
	}
	return !strings.Contains(first, ".")
}

// The entry point must also stay a single file. Nothing about six logic lines
// requires more than one, and a second file is where the next "just the entry
// point" addition lands.
func TestEntrypointIsOneFile(t *testing.T) {
	entries, err := os.ReadDir(entrypointDir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	if len(files) != 1 || files[0] != "main.go" {
		t.Errorf("tools/cmd/llz holds %v; it should hold main.go alone", files)
	}
}

// And it must actually be the entry point — a `main` that stopped calling into
// this package would leave the tree unreachable while every other check here
// still passed.
func TestEntrypointCallsMain(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(entrypointDir, "main.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Main" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "cli" {
			found = true
		}
		return true
	})
	if !found {
		t.Error("tools/cmd/llz/main.go never calls cli.Main() — the binary would build and " +
			"do nothing, which no other check here would notice")
	}
}
