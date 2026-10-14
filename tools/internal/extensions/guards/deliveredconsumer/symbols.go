package deliveredconsumer

// symbols.go builds the "does this symbol still exist" corpus.
//
// DECLARATIONS, NOT MENTIONS, and that distinction is the whole guard.
//
// The first cut of this file collected every identifier appearing anywhere under
// tools/ and asked whether the named consumer was among them. Reconstructing the
// historical case proved it useless: `RenderValues` was retired, and values.go
// still carries the tombstone comment explaining what was removed and why — which
// is this repo's convention and a good one. The symbol was "present", the guard
// passed, and the exact wedge it is named for walked straight through it.
//
// So the corpus is DECLARATION SITES: `func Name`, `func (r T) Name`, and
// `type` / `const` / `var` names, including inside grouped blocks. A retired
// consumer stops being declared even when the prose about it survives, which is
// the state that must fire.
//
// A text scan rather than a package load, still: this gate has to run on a tree
// that does not compile (it runs at `scaffolded`), and loading packages would make
// a build failure indistinguishable from a missing consumer.
//
// QUALIFIED BY PACKAGE, and that was the second thing this got wrong. An
// unqualified corpus of bare names meant a row naming `Classify` was satisfied by
// `baoread.Classify` — a function about OpenBao stderr, unrelated to the manifest
// the row was about. The row was passing on a coincidence, and the symbol it
// actually meant did not exist under that name at all. Every name is now recorded
// as both `Name` and `pkg.Name`, and a row is expected to use the qualified form;
// the bare form stays in the corpus only so an existing row that names a
// package-unique symbol keeps resolving rather than failing on a technicality.

import (
	"errors"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/guardkit"
)

// declRe matches a top-level declaration name: a func (with or without a
// receiver), or a type/const/var. The `(?m)` anchors each alternative to the start
// of a line so an identifier inside a body or a comment cannot satisfy it.
var declRe = regexp.MustCompile(`(?m)^func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)|` +
	`^(?:type|const|var)\s+([A-Za-z_][A-Za-z0-9_]*)`)

// groupedNames returns the names declared inside grouped `const (` / `var (` /
// `type (` blocks, tracking BLOCK CONTEXT rather than matching on indentation.
//
// A REGEX ON INDENTATION CANNOT DO THIS, and two rounds of trying is the evidence.
// The first cut required `=`, which excluded iota members past the first and
// `var ( Foo Bar )`. Loosening it to accept a bare `\tName Type` then admitted
// STRUCT FIELDS and INTERFACE METHODS — `Kind`, `Ref` and `Why` off this
// package's own Consumer struct all resolved as declarations. The comment
// justifying it claimed fields sit at two tabs; they sit at one, exactly like a
// grouped const. Indentation simply does not carry the distinction.
//
// Block context does: a name counts only between a `const (` / `var (` / `type (`
// opener and its closing `)`. That admits every grouped declaration form and no
// struct field, because a struct body opens with `{`, not with one of those three
// keywords.
//
// Why it matters rather than being tidiness: this is the guard's liveness arm.
// A name that resolves by coincidence is a row vouching for a consumer that may
// not exist — which has now been the failure here twice (baoread.Classify, then
// these fields), and is exactly what the guard exists to catch elsewhere.
func groupedNames(text string) []string {
	var out []string
	depth := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if depth == 0 {
			for _, kw := range []string{"const (", "var (", "type ("} {
				if strings.HasPrefix(trimmed, kw) {
					depth = 1
				}
			}
			continue
		}
		if trimmed == ")" {
			depth = 0
			continue
		}
		// Inside a group: the declared name is the first token, and only an
		// exported one is ever named by a Consumers row.
		if m := groupedNameRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// groupedNameRe takes the leading identifier of a line inside a declaration
// group. Anchored at the start of the TRIMMED line, so it never depends on how
// deeply the group itself is nested.
var groupedNameRe = regexp.MustCompile(`^([A-Z][A-Za-z0-9_]*)\b`)

// packageRe reads the package clause so each declaration can also be recorded
// qualified.
var packageRe = regexp.MustCompile(`(?m)^package\s+([A-Za-z_][A-Za-z0-9_]*)`)

// repoSymbolCorpus is the set of identifiers appearing anywhere under tools/.
// The caller treats an EMPTY corpus as a failure rather than as "every symbol is
// missing" — that distinction is the whole fail-closed arm.
func repoSymbolCorpus(repo capability.Repo) (map[string]bool, error) {
	out := map[string]bool{}
	base := guardkit.RepoPath(repo, "tools")
	// An ABSENT tools/ returns the empty corpus rather than a walk error, so the
	// caller's "read no Go source" failure is what the reader sees. A raw lstat
	// error here would be true and useless: it names a path, not the reason every
	// consumer row is about to look missing.
	if _, serr := repo.Stat(base); errors.Is(serr, fs.ErrNotExist) {
		return out, nil
	}
	err := repo.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata holds fixtures, some of which deliberately contain retired
			// names; counting them would let a fixture vouch for a deleted symbol.
			if d.Name() == "testdata" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, rerr := repo.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		text := string(raw)
		pkg := ""
		if m := packageRe.FindStringSubmatch(text); m != nil {
			pkg = m[1]
		}
		record := func(name string) {
			if name == "" {
				return
			}
			out[name] = true
			if pkg != "" {
				out[pkg+"."+name] = true
			}
		}
		for _, m := range declRe.FindAllStringSubmatch(text, -1) {
			for _, g := range m[1:] {
				record(g)
			}
		}
		for _, name := range groupedNames(text) {
			record(name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// corpusOf is the test seam onto repoSymbolCorpus: it builds the same
// capability.Repo the gate builds, so the tests exercise the fenced read path
// rather than a second, more permissive walk of their own.
func corpusOf(t testingT, root string) map[string]bool {
	t.Helper()
	syms, err := repoSymbolCorpus(capability.RepoForGate(Extension(), root))
	if err != nil {
		t.Fatalf("build symbol corpus: %v", err)
	}
	return syms
}

// testingT is the slice of *testing.T corpusOf needs, declared here so the
// helper can live beside the code it wraps without importing testing into the
// production build.
type testingT interface {
	Helper()
	Fatalf(format string, args ...any)
}
