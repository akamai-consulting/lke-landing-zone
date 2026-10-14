package cli

// extensions_init_test.go — every extension that declares dependency wiring must
// have it called.
//
// THE COMPILER CANNOT SEE THIS. An exported `func Init()` that nothing invokes is
// a legal program, so four of them sat un-called after the commands left package
// main. `make deadcode` is report-only in this repo (deliberately — see the
// Makefile), and would not have distinguished these from the benign classes it
// reports, so nothing objected.
//
// What it cost: `llz ci bao-seed-seal-key` died mid-e2e on "SetGitHubSecret not
// installed", and `llz ci rotate-state-passphrase` — whose seams default to
// harmless no-ops — would have reported a successful credential rotation having
// written nothing at all.
//
// A SOURCE scan, not a runtime assertion, because that is what can see the
// property: at runtime an unwired seam is indistinguishable from a wired one
// until something calls it, which is the whole problem.
//
// IT RESOLVES IMPORT ALIASES, and the first cut did not. That version matched the
// text `<dirname>.Init()`, which is wrong in the direction that gets a gate
// switched off: `internal/cli/envtopology.go` imports
// extensions/lifecycle/environments as `envtopoext`, so a correctly-wired package
// imported under an alias would have been reported as unwired. It was found by
// scanning for OTHER instances of this bug and getting a false hit on
// `environments` — the checker's own blind spot showing up in its own audit.
// Matching on the resolved import PATH is what the compiler does, so it is what
// this does.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const extModulePrefix = "github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/"

// TestEveryExtensionInitIsCalled walks internal/extensions for exported Init()
// declarations and requires each one to be invoked from this package.
func TestEveryExtensionInitIsCalled(t *testing.T) {
	declared := declaredInits(t, "../extensions")
	// Fail closed: a walk that found nothing would make this test vacuous, and
	// "no extension declares Init()" is exactly what a broken walk looks like.
	if len(declared) == 0 {
		t.Fatal("no extension declares func Init() — refusing to pass vacuously; if that is " +
			"genuinely true now, delete this test rather than leaving it green over nothing")
	}

	called := calledInits(t, ".")
	if len(called) == 0 {
		t.Fatal("internal/cli calls no <pkg>.Init() at all — extensions_init.go is missing or empty")
	}

	for importPath, file := range declared {
		if !called[importPath] {
			t.Errorf("%s declares func Init() but internal/cli never calls it.\n"+
				"\tAn exported Init nothing invokes compiles fine and leaves every seam in that\n"+
				"\tpackage on its default. Where the default errors you get a loud runtime failure\n"+
				"\t(openbao's cost an e2e round); where it is a harmless no-op you get a command\n"+
				"\tthat REPORTS SUCCESS HAVING DONE NOTHING, which is how a credential rotation\n"+
				"\tsilently rotates nothing. Add the call to extensions_init.go.", file)
		}
	}
}

// declaredInits maps an extension's import path to the file declaring its Init().
func declaredInits(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if perr != nil {
			return perr
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "Init" || len(fn.Type.Params.List) != 0 {
				continue
			}
			// The import path is the directory, relative to internal/extensions.
			rel, rerr := filepath.Rel(root, filepath.Dir(p))
			if rerr != nil {
				return rerr
			}
			out[extModulePrefix+filepath.ToSlash(rel)] = p
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

// calledInits returns the set of extension import paths whose Init() this package
// calls, resolving each file's own import aliases.
func calledInits(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", e.Name(), perr)
		}
		// local identifier -> import path, for this file only.
		local := map[string]string{}
		for _, imp := range f.Imports {
			path, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			name := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			local[name] = path
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Init" {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if path, ok := local[id.Name]; ok {
				out[path] = true
			}
			return true
		})
	}
	return out
}
