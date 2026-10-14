package sourceref

// fileheader_test.go — a file header that names a file must name ITSELF.
//
// THE CONVENTION IS ALREADY UNIVERSAL AND WAS NEVER CHECKED. Most files in
// tools/ open with `// <name>.go — what this is`, which is the first thing a
// reader sees and the only orientation an unfamiliar package offers. Nothing
// compared that name to the file it sits in, so every rename during the
// extraction left the old name behind: `ci_gen_toc.go` heading toc.go,
// `ci_harbor.go` heading two DIFFERENT files in the harbor package, deps.go
// heading promotesync.go. FORTY-NINE files, found by an audit rather than a gate.
//
// It is the same failure shape guard-source-refs exists for — a source reference
// that no longer resolves — one level in: the reference is the file's claim about
// its own identity. That is why it lives in this package rather than beside the
// code it checks.
//
// WHY A TEST AND NOT A `llz ci` VERB. The guard next door reads the WORKING TREE
// through a read-repo capability and reports findings an operator acts on. This
// checks a convention that only ever breaks in a commit a developer is already
// running `go test` on, and it needs no capability, no corpus vocabulary and no
// exemption list. A verb would be machinery for a rule with no runtime audience.
//
// HEADERS ARE OPTIONAL. A file with no `name.go —` opener is not a finding; the
// convention is that IF you name yourself, you do it correctly. Enforcing the
// header's presence would be a different and much larger argument.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// headerRE matches the convention's opening line: `// <file>.go — …` or with an
// ASCII hyphen, which a few files use.
var headerRE = regexp.MustCompile(`^//\s*([A-Za-z0-9_]+\.go)\s*[—-]`)

func TestAFileHeaderNamesItsOwnFile(t *testing.T) {
	toolsDir := filepath.Join(repoRootForTest(t), "tools")

	var bad []string
	var checked, headed int
	err := filepath.WalkDir(toolsDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata holds fixtures that deliberately impersonate other trees.
			if d.Name() == "testdata" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		checked++
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		// The header is the first comment block after `package x`, so only the top
		// of the file is in scope — a mid-file comment mentioning another file is
		// an ordinary cross-reference and guard-source-refs already owns those.
		lines := strings.Split(string(data), "\n")
		if len(lines) > 12 {
			lines = lines[:12]
		}
		for _, ln := range lines {
			m := headerRE.FindStringSubmatch(ln)
			if m == nil {
				continue
			}
			headed++
			if m[1] != d.Name() {
				rel, _ := filepath.Rel(toolsDir, p)
				bad = append(bad, rel+" heads itself \""+m[1]+"\"")
			}
			break
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking tools/: %v", err)
	}

	// FAIL CLOSED, the same arm repo_floors_test.go carries: a walk that found no
	// headers is a broken walk, not a clean tree, and would report green forever.
	if checked < 500 || headed < 200 {
		t.Fatalf("scanned %d Go files and found %d headers — the walk is broken, not the tree "+
			"(a green run here must mean the convention holds, not that nothing was read)", checked, headed)
	}

	for _, b := range bad {
		t.Errorf("%s — a header naming the wrong file is the first thing a reader sees, and it "+
			"sends them to a file that is either gone or is somebody else's", b)
	}
	t.Logf("%d Go files, %d with a self-naming header, %d wrong", checked, headed, len(bad))
}
