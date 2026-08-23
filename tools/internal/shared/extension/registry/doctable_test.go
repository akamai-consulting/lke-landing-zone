package registry

// doctable_test.go — an extension's doc-comment binding table must match the
// bindings it actually declares.
//
// Nearly every extension opens with a tabulated summary of its own declaration:
//
//	transition:scaffolded/definition [read-repo, write-repo]
//	assertion:configured/topology    [read-repo]
//
// It is the first thing a reader meets and the only orientation the package
// offers, and nothing compared it to the declaration forty lines below. An audit
// of the tree found five that had drifted, and the drift ran in the direction
// that matters: `environments` listed `add` and `set` as read-repo when both had
// been corrected to hold write-repo, `template-sustain` and `guard-coverage` each
// omitted a `transition:scaffolded` binding holding write-repo outright, and
// `build-preflight` called an assertion a gate. A summary of a permission model
// that under-reports a WRITE is worse than no summary: it is what a reviewer
// checks instead of the code.
//
// PARSED FROM THE COMMENT, COMPARED AGAINST THE REGISTRY. The left side is the
// tab-indented rows in each package's Go comments; the right is Extension().Bindings
// through registry.Package, so neither side is transcribed and a new binding shows
// up on the right the moment it compiles. Same shape as commands_census_test.go.
//
// A TABLE IS OPTIONAL — a package with no tabulated rows is not a finding, for the
// reason fileheader_test.go gives about file headers: the convention is that IF you
// write one, it is correct. Enforcing presence is a larger argument.
//
// A ROW MARKED `← REFUSED` IS SKIPPED, because it deliberately documents a shape
// the validator rejects rather than a binding that exists — see
// credential-state-passphrase, which shows the state its grant would have taken if
// grantStates allowed it.
//
// COMPARED PER PACKAGE, NOT PER EXTENSION, because one package may declare several
// (credrotate holds credential-pat and credential-objkey, each with its own table).
// Attributing a row to the right extension would need the table tied to the func
// its doc comment sits on; the package-level multiset catches every drift except a
// row swapped between two extensions of the SAME package, which is one package
// today. Stated rather than left as a silent limit.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// docRowRe matches one tabulated row. The binding NAME is optional because a
// single-binding extension has nothing to disambiguate, and the grant list is
// compared as a set so column padding cannot matter.
var docRowRe = regexp.MustCompile(`^(transition|assertion|invariant|gate):([a-z]+)(?:/([a-z0-9-]+))?\s*\[([^\]]*)\]`)

func TestEveryExtensionDocTableMatchesItsDeclaration(t *testing.T) {
	// Keyed by package path as registry.Package spells it (bucket/name), which is
	// the one identifier both sides share.
	declared := map[string][]extension.Binding{}
	for _, e := range All() {
		pkg, ok := Package(e.Name)
		if !ok {
			t.Fatalf("%s: registry.Package could not locate its package — this test compares by package path", e.Name)
		}
		declared[pkg] = append(declared[pkg], e.Bindings...)
	}
	if len(declared) == 0 {
		t.Fatal("no extensions resolved to a package: refusing to pass having compared nothing")
	}

	root := extensionsDir(t)
	found := map[string][]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rows := docRows(string(body))
		if len(rows) == 0 {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(rel)
		found[pkg] = append(found[pkg], rows...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	for pkg, rows := range found {
		bindings, ok := declared[pkg]
		if !ok {
			// A table in a package the registry does not know about is not this
			// test's business (a helper package, a bucket doc).
			continue
		}
		checked++
		sort.Strings(rows)
		want := declaredRows(bindings)
		if strings.Join(rows, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: the doc-comment table does not match the declaration.\n  comment:\n\t%s\n  declared:\n\t%s",
				pkg, strings.Join(rows, "\n\t"), strings.Join(want, "\n\t"))
		}
	}
	// Fail closed on vacuity: the walk finding no tables at all means the
	// convention moved or the parser broke, not that every table is correct.
	if checked == 0 {
		t.Fatal("no extension doc tables were found — this test would pass having examined nothing")
	}
}

// docRows extracts the tabulated binding rows from a file's Go comments, as
// normalised `kind:state[grant,grant]` strings sorted for comparison. The binding
// name is dropped: a table that names its bindings and one that does not are both
// legible, and requiring the name would be a second rule with its own argument.
func docRows(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		// Tab-indented inside a comment is what makes a row a row — Go renders it
		// as a code block, and it is what separates the table from ordinary prose
		// that happens to mention a binding.
		if !strings.HasPrefix(line, "//\t") {
			continue
		}
		row := strings.TrimSpace(strings.TrimPrefix(line, "//\t"))
		if strings.Contains(row, "REFUSED") {
			continue
		}
		m := docRowRe.FindStringSubmatch(row)
		if m == nil {
			continue
		}
		out = append(out, normalise(m[1]+":"+m[2], m[4]))
	}
	sort.Strings(out)
	return out
}

// declaredRows is the same shape, built from the bindings themselves.
func declaredRows(bs []extension.Binding) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		grants := make([]string, 0, len(b.Grants))
		for _, g := range b.Grants {
			grants = append(grants, string(g))
		}
		out = append(out, normalise(string(b.Kind)+":"+string(b.State), strings.Join(grants, ",")))
	}
	sort.Strings(out)
	return out
}

func normalise(head, grants string) string {
	var gs []string
	for _, g := range strings.Split(grants, ",") {
		if g = strings.TrimSpace(g); g != "" {
			gs = append(gs, g)
		}
	}
	sort.Strings(gs)
	return fmt.Sprintf("%s[%s]", head, strings.Join(gs, ","))
}
