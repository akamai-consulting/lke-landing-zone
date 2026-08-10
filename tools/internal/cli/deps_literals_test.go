package cli

// deps_literals_test.go — an Install literal must set every func field its Deps
// declares.
//
// THREE TIMES, SAME DEFECT, THREE DIFFERENT PACKAGES. Every Deps package here
// follows `func Install(d Deps) { caps = d }` — a WHOLESALE REPLACE. So a field
// the composition root's struct literal omits does not fall back to the
// package's carefully fail-closed default; it becomes Go's zero value, nil. The
// next call through it is a SIGSEGV, arbitrarily far from the omission:
//
//	assertreconciler.WithPrometheus     omitted -> scrape-reconciler died rc=2 mid-e2e
//	assertidentity.PortForwardOpenbao   omitted -> team-write died rc=2 mid-e2e, three
//	                                    assert-suite rounds after the first was fixed
//
// Each individual Install now fills omitted fields, which turns the crash into a
// named error. THIS test is the other half: it catches the omission at PR time,
// before anyone spends a cluster on it — and it catches the case filling cannot,
// where the "default" a field falls back to is a no-op that makes the lane pass
// having asserted nothing.
//
// A source scan because that is where the fact lives: at runtime a nil seam is
// indistinguishable from a wired one until something calls it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// funcFieldsOfDeps returns pkg -> set of func-typed field names on its `Deps`.
func funcFieldsOfDeps(t *testing.T, root string) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if n := info.Name(); n == "testdata" || n == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Deps" {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				if _, isFunc := fld.Type.(*ast.FuncType); !isFunc {
					continue
				}
				for _, nm := range fld.Names {
					if !nm.IsExported() {
						continue
					}
					if out[f.Name.Name] == nil {
						out[f.Name.Name] = map[string]bool{}
					}
					out[f.Name.Name][nm.Name] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

// TestEveryDepsLiteralSetsEveryFuncField walks this package for
// `<pkg>.Install(<pkg>.Deps{…})` and requires the literal to name every func
// field that Deps declares.
func TestEveryDepsLiteralSetsEveryFuncField(t *testing.T) {
	fields := funcFieldsOfDeps(t, "../extensions")
	if len(fields) == 0 {
		t.Fatal("found no Deps struct with func fields — refusing to pass vacuously")
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(token.NewFileSet(), e.Name(), nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", e.Name(), perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Deps" {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			want := fields[id.Name]
			if want == nil {
				return true // not a Deps we measured (aliased import of another tree)
			}
			checked++
			set := map[string]bool{}
			for _, el := range lit.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok {
						set[k.Name] = true
					}
				}
			}
			for name := range want {
				if !set[name] {
					t.Errorf("%s: %s.Deps literal does not set %s.\n"+
						"\tInstall REPLACES the whole struct, so an omitted func field is nil — not the\n"+
						"\tpackage's fail-closed default. Calling it is a SIGSEGV, arbitrarily far from\n"+
						"\there: this exact omission killed scrape-reconciler and then team-write in the\n"+
						"\tmiddle of a live e2e. Set it explicitly, even to the package default.",
						e.Name(), id.Name, name)
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no <pkg>.Deps{…} literals found in internal/cli — the scan examined nothing")
	}
	t.Logf("checked %d Deps literal(s) against %d Deps type(s)", checked, len(fields))
}
