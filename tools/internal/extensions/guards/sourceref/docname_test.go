package sourceref

// docname_test.go — a doc comment must not open by naming a DIFFERENT symbol.
//
// THE ORPHANED-COMMENT CLASS. Go's convention is that a declaration's doc opens
// with its own name, so the first word is load-bearing: it is what `go doc` prints
// and what a reader anchors on. When a helper is split out of a function, or a
// function is renamed, or a type declaration moves, the comment stays where it was
// and silently re-attaches to whatever now follows it. The reader is then told,
// authoritatively, that they are looking at something else.
//
// Three were found in tools/, and each had a different cause:
//
//   - openbao.NewClientFor opened "Client builds…" — its own name before the
//     Client TYPE took it, so the doc named a different existing symbol.
//   - bootstrapcluster.aplGitConfigAttempts carried four paragraphs about why
//     exhausting the budget is terminal, which are waitAplGitConfig's; splitting
//     the budget helper out stranded them.
//   - cli.onboardOptsOf carried a stale duplicate of globalOpts' doc that
//     CONTRADICTED the real one on the type's own declaration.
//
// The same shape turned up by hand in reconcile-actions, where inserting a binding
// pushed sc-demote's reason onto the credential rotator.
//
// WHY THE "NEVER MENTIONS ITSELF" NARROWING. Flagging every doc whose first word
// differs from the symbol reports 14, and most are fine: a long block that opens
// with background and names the symbol three paragraphs down is a house style used
// deliberately throughout this tree. The orphan is the case where the function's
// own name appears NOWHERE in its doc — nobody writes that on purpose.
//
// It also only fires when the opening word is a symbol DECLARED IN THE SAME FILE.
// A doc opening with a prose word, or naming something from another package, is
// ordinary writing.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var docLeadRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\b`)

func TestADocCommentDoesNotOpenByNamingAnotherSymbol(t *testing.T) {
	toolsDir := filepath.Join(repoRootForTest(t), "tools")

	var orphans []string
	var files, docs int
	err := filepath.WalkDir(toolsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		// Production files only. Test helpers are renamed constantly and their docs
		// are not a published surface.
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		files++
		f, perr := parser.ParseFile(token.NewFileSet(), p, nil, parser.ParseComments)
		if perr != nil {
			t.Errorf("%s does not parse (%v)", p, perr)
			return nil
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Doc == nil {
				continue
			}
			docs++
			text := fn.Doc.Text()
			m := docLeadRE.FindStringSubmatch(strings.TrimSpace(text))
			if m == nil || m[1] == fn.Name.Name {
				continue
			}
			if !declaredInFile(f, m[1]) {
				continue
			}
			if strings.Contains(text, fn.Name.Name) {
				continue
			}
			rel, _ := filepath.Rel(toolsDir, p)
			orphans = append(orphans, rel+":"+fn.Name.Name+" opens with \""+m[1]+"\"")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking tools/: %v", err)
	}

	if files < 400 || docs < 1000 {
		t.Fatalf("scanned %d production files holding %d documented funcs — the walk is "+
			"broken, not the tree", files, docs)
	}

	for _, o := range orphans {
		t.Errorf("%s and never names itself — a doc that opens with another declared "+
			"symbol has almost certainly been stranded by a split or a rename, and it tells "+
			"the reader they are looking at something they are not", o)
	}
	t.Logf("%d production files, %d documented funcs, %d orphaned docs", files, docs, len(orphans))
}

func declaredInFile(f *ast.File, name string) bool {
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncDecl:
			if d.Name.Name == name {
				found = true
			}
		case *ast.TypeSpec:
			if d.Name.Name == name {
				found = true
			}
		}
		return true
	})
	return found
}
