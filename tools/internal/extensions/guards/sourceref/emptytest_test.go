package sourceref

// emptytest_test.go — a test function with an empty body passes unconditionally.
//
// FOUND ONE, AND THE NAME IS WHY IT SURVIVED. `TestHarborWorkloadSets` covered
// HarborDeployments/HarborStatefulSets; f0aa68f retired those functions along with
// the workflow job that consumed them, and the test was HOLLOWED OUT rather than
// deleted — leaving `func TestHarborWorkloadSets(t *testing.T) {}` in the suite.
// It passed on every run for as long as it existed. What stands in its place is
// TestHarborRegistryDeploymentsIsTheSeededSetOnly, which asserts what survived.
//
// An empty test is worse than a missing one. A missing test is visibly missing; an
// empty one inflates the count, appears in `go test -v` output as a PASS, and reads
// to anyone scanning for coverage as though its subject is covered. Coverage
// tooling cannot see it either — a test that executes no production line simply
// contributes nothing, which is indistinguishable from a test that is not there.
//
// This is the vacuous-green shape every corpus guard in this tree refuses, sitting
// in the test suite rather than in the code under test.
//
// PARSED, NOT GREPPED. A regex reports findings for Go source inside the symbols
// guard's FIXTURE STRINGS; go/ast gets string literals right by construction, which
// is the whole reason to pay for a parser here.
//
// WHAT IT DOES NOT CLAIM. A non-empty body is not a meaningful body — a test whose
// only statement is a call that cannot fail is still vacuous, and this check cannot
// see that. It catches the one shape that is unambiguous and mechanical. The
// mutation-testing lanes are what speak to the rest.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoTestFunctionHasAnEmptyBody(t *testing.T) {
	toolsDir := filepath.Join(repoRootForTest(t), "tools")

	var empty []string
	var files, tests int
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
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		files++
		f, perr := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if perr != nil {
			// A test file this cannot parse is one it cannot vouch for. Same call
			// the source-ref guard makes on an unreadable file.
			t.Errorf("%s does not parse (%v) — the check cannot vouch for a file it "+
				"could not read", p, perr)
			return nil
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil {
				continue
			}
			name := fn.Name.Name
			if !strings.HasPrefix(name, "Test") && !strings.HasPrefix(name, "Fuzz") &&
				!strings.HasPrefix(name, "Benchmark") && !strings.HasPrefix(name, "Example") {
				continue
			}
			tests++
			if len(fn.Body.List) == 0 {
				rel, _ := filepath.Rel(toolsDir, p)
				empty = append(empty, rel+":"+name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking tools/: %v", err)
	}

	// FAIL CLOSED. A walk that found no test files is a broken walk, not a clean
	// tree, and would report green forever.
	if files < 200 || tests < 800 {
		t.Fatalf("scanned %d test files holding %d test funcs — the walk is broken, not "+
			"the tree", files, tests)
	}

	for _, e := range empty {
		t.Errorf("%s has an empty body — it passes unconditionally and reads as coverage. "+
			"Either assert what survived of its subject, or delete it; a missing test is "+
			"visibly missing, an empty one is not.", e)
	}
	t.Logf("%d test files, %d test funcs, %d empty", files, tests, len(empty))
}
