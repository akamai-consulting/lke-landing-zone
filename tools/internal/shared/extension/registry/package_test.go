package registry

// package_test.go — every extension must be locatable from its name.
//
// THE NAME IS THE ONLY HANDLE A READER HAS. Every error message, gate exemption
// and ratchet entry in this tree names the EXTENSION — `posture-plaintext`,
// `assert-storage`, `import-brownfield` — and for fifteen of the sixty-two the
// package is called something else entirely (guards/plaintext, assertions/volumes,
// lifecycle/brownfield). Package() is what closes that, so a Package() that
// silently returned "" for some extension would leave exactly the readers it was
// added for with nothing.

import (
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
}
