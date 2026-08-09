package sourceref

// symbols.go implements `llz ci symbol-ref-guard`: fail CI when prose names a
// `pkg.Symbol` that the package does not export.
//
// THE SIBLING DEFECT TO A STALE PATH, and the one that survives fixing paths: a
// reference can name the right FILE and the wrong SYMBOL, which is what a move
// between packages leaves behind once the compiler has forced every CALLER to be
// updated and left every COMMENT alone.
//
// ────────────────────────────────────────────────────────────────────────────
// THE CONVENTION THIS GUARD ENFORCES, because it is not one Go already has:
//
//	pkg.Symbol   a LIVE pointer. It resolves today, and a reader may follow it.
//	pkg's Symbol HISTORY. Where something used to be, what it used to be called.
//
// WITHOUT THAT SPLIT THE GUARD IS UNSHIPPABLE HERE, and the first run proved it
// rather than argued it: fifteen findings, and every one was a sentence
// deliberately naming a symbol at its OLD home — "it USED TO READ
// selfupgrade's LatestRelease", "cli's ParseRotatorArgs outlived its callers and
// has been removed", two of them verbatim quotations of older comments. Not one
// was a mistake. A repo that documents its scars will write those sentences
// forever, and a guard that forbids them is a guard that gets deleted.
//
// The possessive costs an apostrophe and reads better than the alternative,
// which is what makes it hold: a dotted reference now MEANS something a reader
// can act on, and the guard is what keeps that promise true. Compare the path
// guard's version of the same rule — name the symbol, say where it moved, do not
// leave a pointer that only looks live.
//
// A REFERENCE TO A LOCAL VARIABLE takes the same treatment for the same reason.
// A comment asserting that a parameter's exported field is non-nil, where that
// parameter is named for a package that also exists here, is ambiguous to the
// guard AND to a reader; the fix is "the platform argument's Components", naming
// the argument rather than dotting off a bare word, which answers both.
// (Spelled around rather than quoted — this guard reads its own source, and it
// cannot tell an illustration from a claim.)
//
// ────────────────────────────────────────────────────────────────────────────
// IN GO FILES THIS READS COMMENTS ONLY, AND THAT IS THE WHOLE DESIGN.
//
// The naive reading — scan the file's text — measures 4,013 `testing.T`, 1,913
// `strings.Contains` and 1,512 `fmt.Errorf`, none of which this repo owns, plus
// the genuinely dangerous shape: a LOCAL VARIABLE whose name is one of our
// package names. Sixteen of the 142 packages under tools/internal are called
// things like `repo`, `registry`, `render`, `health`, `metrics` and `validate`,
// and `repo.ReadFile` in a function body is a variable, not a package
// reference. Judging those would need type resolution — the whole go/types
// apparatus — to answer a question the guard never has to ask, because the rot
// it exists to catch is never in code. Code that names a moved symbol does not
// compile. Only the COMMENT beside it rots silently.
//
// So Go input arrives through go/parser and only ast.Comment text is scanned.
// That removes the local-variable class completely rather than approximating it,
// and it is why the compiler is left to do the job it already does.
//
// ────────────────────────────────────────────────────────────────────────────
// AND IT ONLY JUDGES PACKAGES WE OWN. A reference resolves against the symbol
// table built from this repo's own Go tree; a package name absent from it is
// skipped, not reported. `cobra.Command` and `color.Green` are somebody else's
// API and this guard has no business having an opinion about them — it cannot
// see their source, so a finding would only ever mean "not vendored here".

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// symExpr matches `pkg.Symbol`. The package half is lowercase alphanumeric — Go
// package-name shape — which is what keeps prose out: `Fig.2` and `README.MD`
// fail the first half, `e.g.` and `openbao.go` fail the second.
//
// The leading group stands in for a lookbehind Go's regexp does not have. It
// ADMITS `/`, unlike the path guard's: an ldflag names a symbol as
// `…/tools/internal/cli.Version`, and the segment before the dot is the package
// there just as much as it is after a space.
// A `*` is admitted in the symbol half and treated as a GLOB, matching the path
// guard: this repo abbreviates a family of related symbols as one reference —
// `versionpins.CI*Tag` for CITofuTag and CIKubernetesTag — and reading that as a
// symbol literally called "CI" reported it as stale while both members existed.
// Requiring at least one match keeps the check: an abbreviation whose whole
// family has been renamed still fails.
var symExpr = regexp.MustCompile(`(^|[^A-Za-z0-9_.])([a-z][a-z0-9]*)\.([A-Z][A-Za-z0-9_*]*)`)

// SymbolReport is what a run examined. Exported for the same reason Report is:
// the coverage numbers are what separate a real green from a broken corpus.
type SymbolReport struct {
	Packages int // packages whose exported surface was indexed
	Symbols  int // exported identifiers in the table
	Scanned  int // files read
	Refs     int // pkg.Symbol references judged
	Findings []Finding
}

// RunSymbols scans root and fails when a reference names a symbol its package
// does not export.
func RunSymbols(root string) error {
	repo := capability.RepoForGate(Extension(), root)

	table, err := symbolTable(repo)
	if err != nil {
		return fmt.Errorf("symbol-ref-guard: %w", err)
	}

	files, err := corpus(repo)
	if err != nil {
		return fmt.Errorf("symbol-ref-guard: %w", err)
	}

	rep := SymbolReport{Packages: len(table)}
	for _, syms := range table {
		rep.Symbols += len(syms)
	}

	for _, rel := range files {
		data, err := repo.ReadFile(rel)
		if err != nil {
			return fmt.Errorf("symbol-ref-guard: %s could not be read (%w) — "+
				"the guard cannot vouch for a file it did not open", rel, err)
		}
		rep.Scanned++

		lines, err := scannableLines(rel, data)
		if err != nil {
			return fmt.Errorf("symbol-ref-guard: %s: %w", rel, err)
		}
		for _, ln := range lines {
			for _, m := range symExpr.FindAllStringSubmatch(ln.text, -1) {
				pkg, sym := m[2], m[3]
				syms, ours := table[pkg]
				if !ours {
					continue // somebody else's API; we cannot see its source
				}
				rep.Refs++
				if !symbolKnown(syms, sym) {
					rep.Findings = append(rep.Findings, Finding{
						File: rel, Line: ln.num, Ref: pkg + "." + sym,
					})
				}
			}
		}
	}

	sort.Slice(rep.Findings, func(i, j int) bool {
		if rep.Findings[i].File != rep.Findings[j].File {
			return rep.Findings[i].File < rep.Findings[j].File
		}
		return rep.Findings[i].Line < rep.Findings[j].Line
	})
	for _, f := range rep.Findings {
		fmt.Printf("::error file=%s,line=%d::%s is not exported by any package named %q — "+
			"the symbol was renamed or moved; name the one that exists now\n",
			f.File, f.Line, f.Ref, strings.SplitN(f.Ref, ".", 2)[0])
	}
	if len(rep.Findings) > 0 {
		return fmt.Errorf("symbol-ref-guard: %d stale symbol reference(s) across %d file(s)",
			len(rep.Findings), countFiles(rep.Findings))
	}

	// FAIL CLOSED ON THREE AXES, one more than the path guard, because this one
	// can also be vacuously green by indexing nothing: an empty symbol table makes
	// every reference "somebody else's" and skips the lot.
	if rep.Packages == 0 || rep.Symbols == 0 {
		return fmt.Errorf("symbol-ref-guard: indexed %d package(s) and %d symbol(s) under %q — "+
			"with an empty table every reference looks third-party and is skipped, so this "+
			"fails rather than reporting a green it did not earn", rep.Packages, rep.Symbols, root)
	}
	if rep.Scanned == 0 {
		return fmt.Errorf("symbol-ref-guard: no scannable file under %q — a guard that "+
			"examined nothing cannot report that everything is fine. Check --root", root)
	}
	if rep.Refs == 0 {
		return fmt.Errorf("symbol-ref-guard: %d file(s) scanned against %d symbol(s) but not one "+
			"pkg.Symbol reference found — this repo's headers cite their own API constantly, "+
			"so zero means the extraction is broken", rep.Scanned, rep.Symbols)
	}

	fmt.Printf("symbol-ref-guard: %d symbol reference(s) across %d file(s) all resolve "+
		"(%d package(s), %d exported symbol(s) indexed).\n",
		rep.Refs, rep.Scanned, rep.Packages, rep.Symbols)
	return nil
}

// symbolKnown reports whether sym names something the package exports. A `*`
// makes it a pattern over the exported surface, which must match at least one
// name — the same rule the path guard applies to a glob, and for the same reason:
// a pattern that matches nothing is indistinguishable from a name that is gone.
func symbolKnown(syms map[string]bool, sym string) bool {
	if !strings.Contains(sym, "*") {
		return syms[sym]
	}
	for name := range syms {
		if ok, err := path.Match(sym, name); err == nil && ok {
			return true
		}
	}
	return false
}

// line is one unit of scannable text with the position to report it at.
type line struct {
	num  int
	text string
}

// scannableLines returns the text to judge. Go files yield their COMMENTS; every
// other type yields all of its lines. See this file's header for why.
func scannableLines(rel string, data []byte) ([]line, error) {
	if !strings.HasSuffix(rel, ".go") {
		var out []line
		for i, t := range strings.Split(string(data), "\n") {
			out = append(out, line{num: i + 1, text: t})
		}
		return out, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, data, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		// Unparseable Go is the compiler's finding, not this guard's — and it
		// cannot be a silent skip either, since the file's comments go unchecked.
		// gofmt/vet run before this in every lane, so reaching here means
		// something upstream already failed and should be fixed there.
		return nil, fmt.Errorf("parsing for comments: %w", err)
	}
	var out []line
	for _, g := range f.Comments {
		for _, c := range g.List {
			out = append(out, line{num: fset.Position(c.Pos()).Line, text: c.Text})
		}
	}
	return out, nil
}

// symbolTable maps a package NAME to the exported top-level identifiers declared
// under it, unioned across every directory with that name.
//
// UNIONED ON PURPOSE. Two packages here are called `openbao` — internal/shared
// and extensions/lifecycle — and prose that writes `openbao.Client` does not
// disambiguate and should not have to. The union is the reading a human gives it,
// and the alternative (resolving the intended import) is type-checking a comment.
func symbolTable(repo capability.Repo) (map[string]map[string]bool, error) {
	table := map[string]map[string]bool{}
	files, err := corpus(repo)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	for _, rel := range files {
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		data, err := repo.ReadFile(rel)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		f, err := parser.ParseFile(fset, rel, data, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		pkg := f.Name.Name
		if table[pkg] == nil {
			table[pkg] = map[string]bool{}
		}
		for _, name := range exportedDecls(f) {
			table[pkg][name] = true
		}
	}
	return table, nil
}

// exportedDecls returns every exported identifier a file declares: funcs, types,
// vars, consts, and METHODS, including every name in a grouped or multi-name
// declaration.
//
// MISSING A DECLARATION KIND WOULD BE A FALSE POSITIVE, not a miss, which is why
// this handles the grouped and multi-name forms explicitly rather than the common
// one: a `var ( A = … ; B = … )` block read as a single spec would leave B
// undeclared in the table and report every honest reference to it.
//
// METHODS ARE IN, AND THE FIRST DRAFT LEFT THEM OUT. The reasoning for excluding
// them was that a method is reached as `pkg.Type.Method`, so the first two
// segments already resolve via the type — true of the notation, false of the
// prose. This repo writes `clusterspec.AplChartVersionWarnings` and
// `clusterspec.EmitOnManaged` for methods on LandingZone and Component, naming
// the package a reader would go to rather than the receiver, and that is the
// normal way to cite a method in a sentence. Both were reported by the first run.
// A method name is unambiguously part of its package's exported surface, so
// admitting it costs no precision the guard was relying on.
func exportedDecls(f *ast.File) []string {
	var out []string
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Name.IsExported() {
				out = append(out, d.Name.Name)
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					if s.Name.IsExported() {
						out = append(out, s.Name.Name)
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.IsExported() {
							out = append(out, n.Name)
						}
					}
				}
			}
		}
	}
	return out
}
