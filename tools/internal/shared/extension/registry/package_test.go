package registry

// package_test.go — every extension must be locatable from its name.
//
// THE NAME IS THE ONLY HANDLE A READER HAS. Every error message, gate exemption
// and ratchet entry in this tree names the EXTENSION — `posture-plaintext`,
// `assert-storage`, `import-brownfield` — and for thirty-one of the sixty-two the
// package is called something else entirely (guards/plaintext, assertions/volumes,
// lifecycle/brownfield). Package() is what closes that, so a Package() that
// silently returned "" for some extension would leave exactly the readers it was
// added for with nothing.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryExtensionResolvesToADirectoryThatExists(t *testing.T) {
	root := extensionsDir(t)

	var missing, wrong []string
	for _, e := range All() {
		pkg, ok := Package(e.Name)
		if !ok || pkg == "" {
			missing = append(missing, e.Name)
			continue
		}
		// The answer must be a real directory holding Go source — a path that
		// resolves to nothing is worse than no answer, because a reader follows it.
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		entries, err := os.ReadDir(dir)
		if err != nil {
			wrong = append(wrong, e.Name+" -> "+pkg+" ("+err.Error()+")")
			continue
		}
		var hasGo bool
		for _, en := range entries {
			if strings.HasSuffix(en.Name(), ".go") {
				hasGo = true
				break
			}
		}
		if !hasGo {
			wrong = append(wrong, e.Name+" -> "+pkg+" (no Go source)")
		}
	}

	if len(missing) > 0 {
		t.Errorf("%d extension(s) resolve to no package: %s\n"+
			"\tPackage() derives this from the declaration's constructor, so a miss means the "+
			"constructor is not where the registry thinks — the reader is left with a name and "+
			"no route to the code.", len(missing), strings.Join(missing, ", "))
	}
	if len(wrong) > 0 {
		t.Errorf("%d extension(s) resolve to a path that is not a Go package:\n\t%s",
			len(wrong), strings.Join(wrong, "\n\t"))
	}
}

// An unknown name must say so rather than guessing. The listing calls this for
// every row; a fabricated path is a reader sent to a directory that is not there.
func TestPackageRefusesAnUnknownName(t *testing.T) {
	if pkg, ok := Package("no-such-extension"); ok || pkg != "" {
		t.Errorf("Package(unknown) = %q, %v — want \"\", false", pkg, ok)
	}
}

// THE MISMATCHES ARE THE POINT, so at least one must exist: if package names ever
// equalled extension names everywhere, Package() would be answering a question
// nobody has and should be deleted rather than kept out of habit.
func TestSomeExtensionNamesDifferFromTheirPackage(t *testing.T) {
	var differ int
	for _, e := range All() {
		pkg, ok := Package(e.Name)
		if !ok {
			continue
		}
		leaf := pkg
		if i := strings.LastIndex(pkg, "/"); i >= 0 {
			leaf = pkg[i+1:]
		}
		if leaf != strings.ReplaceAll(e.Name, "-", "") {
			differ++
		}
	}
	if differ == 0 {
		t.Error("every extension's package leaf now matches its name — Package() has no " +
			"question left to answer and the listing should drop it")
	}
	t.Logf("%d of %d extensions live in a package whose name differs from theirs", differ, len(All()))

	// THE COUNT IS PINNED BECAUSE IT WAS ONLY EVER LOGGED, and that is exactly how it
	// rotted. Three comments quote this number to justify Package()'s existence — and
	// all three said "fifteen" while this test was measuring thirty-one and printing
	// it to a log nobody reads. A number measured but not compared is not a
	// measurement, it is a footnote.
	//
	// Bumping it is fine and expected; updating the three sites below in the same
	// commit is the point. They are the whole population — `grep -rn "of the
	// seventy-three"` finds them.
	const documented = 39 // registry.go (Package), package_test.go (above), cli/extension.go (listVerbose)
	if differ != documented {
		t.Errorf("%d extensions differ from their package name; the comments justifying Package() say %d.\n"+
			"\tUpdate all three together — registry.go's Package doc, this file's header, and "+
			"cli/extension.go's listVerbose comment — then bump `documented` here.", differ, documented)
	}
	if total := len(All()); total != 73 {
		t.Errorf("the registry holds %d extensions; the same three comments say seventy-three. "+
			"Same rule: update the prose with the set.", total)
	}
}

// ── the census sites nothing compared ───────────────────────────────────────

// repoRootForCensus walks up from the package dir to the repo root (the dir
// holding docs/designs/), so the test is independent of where `go test` runs.
func repoRootForCensus(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "docs", "designs")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("repo root not reachable from here")
	return ""
}

// TestProseCensusSitesAgreeWithTheRegistry.
//
// package_test.go above pins the three comments that justify Package(); this pins
// the four that state the SIZE of the model. They were unpinned, so
// `make lint LINT_ALL=1` stayed green while two design docs and a runtime help
// string said 68 extensions / 29 gates against a registry holding 69 and 30 —
// found by a code review, which is not a mechanism.
//
// Asserting the NUMBER IS PRESENT rather than parsing the sentence: these are
// prose, and pinning their wording would make every rephrase a test failure. What
// must not drift is the figure.
func TestProseCensusSitesAgreeWithTheRegistry(t *testing.T) {
	root := repoRootForCensus(t)
	exts, gates := len(All()), len(Gates())

	for _, tc := range []struct {
		file string
		want []string
		why  string
	}{
		{"docs/designs/internal-extension-model.md",
			[]string{fmt.Sprintf("%d extensions", exts)},
			"the design doc's own headline count"},
		{"docs/designs/README.md",
			[]string{fmt.Sprintf("%d extensions", exts), fmt.Sprintf("%d gate rows", gates)},
			"the index row that summarises the model"},
		{"tools/internal/extensions/guards/k8sminorcoherence/extension.go",
			nil, // asserted by ABSENCE below — it must not restate a count
			"a runtime help string that quoted the gate count"},
	} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(tc.file)))
		if err != nil {
			t.Errorf("read %s: %v", tc.file, err)
			continue
		}
		body := string(b)
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s no longer says %q (%s). The registry holds %d extensions and %d gates; "+
					"update the prose in the same commit that changed the set — a count nothing "+
					"compares is a footnote, not a measurement.", tc.file, w, tc.why, exts, gates)
			}
		}
	}

	// The k8sminorcoherence help string used to say "all twenty-nine gates". A
	// spelled-out count inside a runtime string cannot be pinned by a number
	// search, so the rule for that one is simpler: do not state a count there.
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(
		"tools/internal/extensions/guards/k8sminorcoherence/extension.go")))
	if err != nil {
		t.Fatalf("read k8sminorcoherence/extension.go: %v", err)
	}
	for _, spelled := range []string{"twenty-eight gates", "twenty-nine gates", "thirty gates", "thirty-one gates"} {
		if strings.Contains(string(b), spelled) {
			t.Errorf("k8sminorcoherence/extension.go states %q in a runtime help string. It is not "+
				"reachable by a count-bearing test, and it went stale exactly that way — say "+
				"\"every gate in the driver\" instead of a number", spelled)
		}
	}
}
