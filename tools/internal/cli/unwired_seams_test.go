package cli

// unwired_seams_test.go — every seam whose default says "not wired" must be wired.
//
// THE GENERALISATION OF extensions_init_test.go, and it exists because that test
// was too narrow by exactly one class. It checks exported `func Init()`; the
// FIFTH unwired seam was not an Init at all but a package-level var:
//
//	reconcilelanes.BaoHTTPClient
//	  default: "reconcilelanes: BaoHTTPClient was never wired — package main
//	            assigns it at startup"
//
// That sentence stopped being true when the CLI left package main. Nothing
// assigned it, so every OpenBao call the reconciler's lanes make failed at the
// transport — and it presented four layers away, as `llz ci converge` waiting out
// its full 1200s budget reporting "obj chain settling" on a chain that was not
// settling. It cost four e2e rounds and a kept cluster to find, because the one
// log line naming it was not collected by anything.
//
// The shape is what makes it checkable: a seam that fails closed announces itself
// in its own default. So the corpus is "package-level vars whose default returns
// an error saying it is not wired", and the assertion is that something assigns
// each one. A grep would be close enough to look right and wrong in the usual
// ways (a mention in a comment, an assignment in a test), so this parses.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sentinelPhrases mark a default that exists to be replaced. Kept broad on
// purpose: both spellings the tree uses today ("not installed", "never wired")
// plus the obvious variants, so a new seam is covered by writing an honest error
// rather than by remembering this list.
var sentinelPhrases = []string{"not installed", "never wired", "not wired", "was never assigned"}

// seam is one package-level var whose default announces it is unwired.
type seam struct{ pkg, name, file string }

func TestEverySentinelSeamIsWired(t *testing.T) {
	const root = ".."

	seams := findSentinelSeams(t, root)
	if len(seams) == 0 {
		t.Fatal("no sentinel-default seams found — refusing to pass vacuously; if the " +
			"convention has genuinely gone away, delete this test rather than leaving it green over nothing")
	}

	assigned := findAssignments(t, root)

	for _, s := range seams {
		// Either the composition root assigns it as `pkg.Name = …`, or the seam's
		// own package assigns it from an installer (openbao.Install does this, and
		// that installer is itself held by TestEveryExtensionInitIsCalled).
		if assigned[s.pkg+"."+s.name] || assigned[s.pkg+"|"+s.name] {
			continue
		}
		t.Errorf("%s declares %s with a self-describing \"unwired\" default and NOTHING assigns it.\n"+
			"\tA seam like this fails closed, which is right — but only if something wires it. Left\n"+
			"\tunassigned it fails at the point of use, arbitrarily far from the cause: BaoHTTPClient\n"+
			"\tsurfaced as converge burning a 1200s budget on \"obj chain settling\", four layers and\n"+
			"\tfour e2e rounds away. Wire it in internal/cli/extensions_init.go.", s.file, s.name)
	}
}

// findSentinelSeams walks for `var Name = func(...) ... { … "not installed" … }`.
func findSentinelSeams(t *testing.T, root string) []seam {
	t.Helper()
	var out []seam
	forEachGoFile(t, root, func(path string, f *ast.File, src []byte) {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) || !name.IsExported() {
						continue
					}
					lit, ok := vs.Values[i].(*ast.FuncLit)
					if !ok {
						continue
					}
					body := string(src[lit.Pos()-1 : lit.End()-1])
					if hasSentinel(body) {
						out = append(out, seam{pkg: f.Name.Name, name: name.Name, file: filepath.ToSlash(path)})
					}
				}
			}
		}
	})
	return out
}

// findAssignments records both `pkg.Name = …` (cross-package) and `Name = …`
// (same-package, keyed with a `|` so the two cannot be confused).
func findAssignments(t *testing.T, root string) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	forEachGoFile(t, root, func(_ string, f *ast.File, _ []byte) {
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range as.Lhs {
				switch v := lhs.(type) {
				case *ast.SelectorExpr:
					if id, ok := v.X.(*ast.Ident); ok {
						got[id.Name+"."+v.Sel.Name] = true
					}
				case *ast.Ident:
					got[f.Name.Name+"|"+v.Name] = true
				}
			}
			return true
		})
	})
	return got
}

// forEachGoFile visits every non-test .go file under root.
func forEachGoFile(t *testing.T, root string, fn func(path string, f *ast.File, src []byte)) {
	t.Helper()
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if n := info.Name(); n == "testdata" || n == "worktrees" || n == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		f, perr := parser.ParseFile(token.NewFileSet(), p, src, 0)
		if perr != nil {
			return perr
		}
		fn(p, f, src)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

func hasSentinel(s string) bool {
	for _, p := range sentinelPhrases {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
