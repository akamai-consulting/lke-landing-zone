package sourceref

// testcite_test.go — a comment naming a test must name a test that exists.
//
// THIS TREE CITES TESTS AS EVIDENCE. "pinned by TestGrantStatesTableIsPinned",
// "TestUndrivenGatesMatchTheModel compares both directions", "there is a test that
// will notice" — the citation IS the argument that a rule is enforced rather than
// merely written down. AGENTS.md asks a PR to name the gate; these comments are
// where that answer lives afterwards.
//
// So a citation that no longer resolves is not a typo. It is a claim of coverage
// with nothing behind it, and it reads exactly like a real one. Three were found:
//
//   - clusterspec cited TestHAGroupMissingRole; the test is TestHaGroupMissingRole.
//   - bootstrapcluster's doc opened "TestLlzOpenbaoNamespaceManifest"; the function
//     under it is TestLlzNamespaceManifest.
//   - plaintext's guard said "same rationale as TestSchemeHTTPSpellings", which has
//     never existed under that name — the reader is sent to find the reasoning and
//     finds nothing.
//
// NEAR-MATCHES ARE ALLOWED, and that is not laxity. Table-driven tests are cited by
// their stem all over this tree (TestKeycloakConnect_ for a family of
// TestKeycloakConnect_Foo), so a citation that is a prefix of a real test, or of
// which a real test is a prefix, resolves. Only a name with no relative anywhere in
// the suite is a finding.
//
// HISTORICAL CITATIONS ARE ALLOWED TOO, by the same rule the package-main sweep
// followed: "TestFilesProducesTokenFreeTF is the former TestRenderWritesTokenFreeTF"
// and "the name of this test USED TO BE TestOnlyDestroyMutates" are correct as
// written, and both survive here because the surviving name is in the same comment.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	testDeclRE = regexp.MustCompile(`(?m)^func ((?:Test|Fuzz|Benchmark|Example)\w+)\(`)
	testCiteRE = regexp.MustCompile(`\b((?:Test|Fuzz|Benchmark)[A-Z]\w{4,})\b`)
)

func TestACitedTestNameResolvesToARealTest(t *testing.T) {
	toolsDir := filepath.Join(repoRootForTest(t), "tools")

	// Every test the suite actually declares.
	real := map[string]bool{}
	walkGoFiles(t, toolsDir, func(p string, data string) {
		if !strings.HasSuffix(p, "_test.go") {
			return
		}
		for _, m := range testDeclRE.FindAllStringSubmatch(data, -1) {
			real[m[1]] = true
		}
	})
	if len(real) < 1000 {
		t.Fatalf("found only %d test declarations — the walk is broken, not the tree", len(real))
	}

	resolves := func(name string) bool {
		if real[name] {
			return true
		}
		for r := range real {
			if strings.HasPrefix(r, name) || strings.HasPrefix(name, r) {
				return true
			}
		}
		return false
	}

	// BLOCK-LEVEL, NOT LINE-LEVEL. The legitimate shape is "TestFoo is the former
	// TestBar" — a rename recorded so the next reader can follow it — and the dead
	// name is exactly what makes that sentence useful. Judged per line it is a
	// finding; judged per contiguous comment block it is self-resolving, because the
	// live name sits beside it.
	//
	// So the rule is: a block may name a dead test as long as it also names a live
	// one. That is not a loophole, it is the property worth having — it forces a
	// comment mentioning something gone to say where the reader should go instead.
	var bad []string
	walkGoFiles(t, toolsDir, func(p string, data string) {
		if !strings.HasSuffix(p, ".go") {
			return
		}
		lines := strings.Split(data, "\n")
		for i := 0; i < len(lines); i++ {
			if !strings.HasPrefix(strings.TrimSpace(lines[i]), "//") {
				continue
			}
			start := i
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "//") {
				i++
			}
			block := strings.Join(lines[start:i], "\n")
			var dead []string
			var live bool
			for _, m := range testCiteRE.FindAllStringSubmatch(block, -1) {
				if resolves(m[1]) {
					live = true
					continue
				}
				dead = append(dead, m[1])
			}
			if live || len(dead) == 0 {
				continue
			}
			rel, _ := filepath.Rel(toolsDir, p)
			bad = append(bad, rel+":"+strconv.Itoa(start+1)+" cites "+strings.Join(dead, ", "))
		}
	})

	for _, b := range bad {
		t.Errorf("%s, which no test declares — in this tree a named test IS the claim "+
			"that a rule is enforced, so an unresolvable one is a coverage claim with "+
			"nothing behind it", b)
	}
	t.Logf("%d declared tests, %d unresolvable citations", len(real), len(bad))
}

func walkGoFiles(t *testing.T, root string, fn func(path, data string)) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		fn(p, string(b))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}
