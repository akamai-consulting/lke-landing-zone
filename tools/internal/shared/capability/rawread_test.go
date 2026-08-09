package capability_test

// RAW os.ReadFile IS THE HOLE THE read-repo FENCE CANNOT SEE, and unlike the
// kubectl case there was never a seam to miss: `read-repo` is declared by 40 of
// 61 extensions and every one of them reached the standard library directly —
// 124 os.ReadFile, 44 os.Stat, 12 os.ReadDir, 11 filepath.WalkDir. The grant was
// review metadata, and `llz ci gates`'s claim that a gate "touches nothing but
// files" was enforced by a check on the DECLARATION (extension.checkBindingCeiling)
// and by nothing at runtime.
//
// This guard watches what the grant cannot. It counts the raw filesystem calls
// per package and fails when a new one appears — and, like the in-degree and raw
// kubectl ratchets, it fails in BOTH directions, so a conversion has to be banked
// rather than left as room to regrow.
//
// ────────────────────────────────────────────────────────────────────────────
// IT WATCHES guards/ ONLY, AND THAT IS A MEASURED CEILING RATHER THAN A
// CONVENIENT ONE.
//
// The 40 declaring packages were converted one bucket at a time because a
// half-converted tree is the dangerous state: a fence that exists and does not
// hold reads as protection. guards/ went first because a gate is the thing that
// runs in a pre-commit hook on a developer's laptop, over a tree that may have
// arrived from a pull request — and because guardwalk/guardkit were already the
// shared file-walking layer for ten of the fifteen, so there WAS something to
// intercept.
//
// All fifteen are fenced, plus the three non-guard guardwalk callers the shared
// walk dragged along (atrest, manifestguard, assertobs). assertions/ and
// lifecycle/ are NOT yet, which is why widening this walk to internal/extensions
// would fail on day one for a reason everyone already knows — the shape the
// layering guard's registry exemption records as "a guard that failed on day one
// for a known reason would simply have been deleted".
//
// THE TWO INDIRECT HOLES ARE NOW CLOSED. This note used to name pincoherence and
// templatemanifest as reaching the filesystem through shared packages that were
// themselves unconverted (internal/shared/answers, internal/shared/manifest) —
// invisible to a ratchet that counts DIRECT calls. Both now read through
// answers.ReadFrom and manifest.RunFrom, and TestGuardsDoNotReadThroughUnfenced
// below keeps them there.
//
// Those two packages keep their unfenced string-taking entry points, and that is
// deliberate rather than half-done: their other callers are internal/verbs, which
// declares no bindings AT ALL by design (shared/extension/verbs_test.go pins it),
// plus cmd/llz. A verb has no binding to build a reader from and often no repo to
// be fenced to — `llz new` writes a tree that does not exist yet. The capability
// model does not reach the CLI's own surface, so the honest boundary is the one
// drawn here rather than a reader minted for callers the model does not cover.
// ────────────────────────────────────────────────────────────────────────────

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// allowedRawRead is every remaining direct filesystem call under
// internal/extensions/guards, by package, with why it is still raw.
//
// IT IS EMPTY, AND AN EMPTY MAP IS THE POINT. Every one of the fifteen guards now
// reads through capability.Repo. A new entry here is not a bookkeeping change —
// it is a gate that can read outside the repository, and it needs the reason
// written next to it.
var allowedRawRead = map[string]int{}

// rawReadCalls matches the four calls the census found. os.Open is included
// because it is the obvious way around a ReadFile ban, and filepath.Walk /
// WalkDir because the walk is where a symlink escape actually bites.
var rawReadCalls = regexp.MustCompile(
	`\bos\.(ReadFile|Stat|Lstat|ReadDir|Open|OpenFile)\(|\bfilepath\.(Walk|WalkDir)\(`)

func TestNoNewRawFilesystemReadsInGuards(t *testing.T) {
	root := filepath.FromSlash("../../extensions/guards")
	got := map[string]int{}

	// THE PATTERN MUST STILL MATCH A CONTROL, for the reason rawcloud_test.go sets
	// out at length: a pattern-scanning ratchet is guarded against a stale regex
	// only by its own outstanding debt, and this allowlist is EMPTY because every
	// guard was converted. Paying the debt off removed the safety net — verified by
	// renaming the pattern, after which this test stayed green over a tree it was
	// no longer reading.
	//
	// repo.go is where the fenced reads actually happen, so the calls this pattern
	// describes must exist there. If they do not, the pattern moved rather than the
	// guards getting clean.
	if control, err := os.ReadFile(filepath.FromSlash("repo.go")); err != nil {
		t.Fatalf("reading the control file: %v", err)
	} else if !rawReadCalls.Match(control) {
		t.Fatal("the raw-read pattern matches nothing in repo.go, where the fenced reads are " +
			"implemented — it now finds nothing anywhere and would report every guard clean " +
			"while reading none of them")
	}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// COMMENTS DO NOT COUNT. Several guards' headers narrate the conversion
		// ("it was `b, _ := os.ReadFile(f)`, which…"), and counting that prose
		// would make the ratchet fail on a documentation change — the fastest way
		// to teach people to delete a guard.
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			n += len(rawReadCalls.FindAllString(line, -1))
		}
		if n > 0 {
			// .../guards/<pkg>/file.go — take the second-to-last segment. The raw
			// kubectl ratchet records why: a path index is only as stable as the
			// layout it assumes, and taking the FIRST segment silently started
			// counting per bucket when the tree sub-divided.
			rel, _ := filepath.Rel(root, path)
			seg := strings.Split(filepath.ToSlash(rel), "/")
			got[seg[len(seg)-2]] += n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for pkg, n := range got {
		want, ok := allowedRawRead[pkg]
		if !ok {
			t.Errorf("guards/%s makes %d direct filesystem call(s) — that bypasses the read-repo "+
				"fence entirely, so this gate can read ~/.aws/credentials while declaring it "+
				"touches nothing but the repo. Route it through capability.Repo (RepoForGate, "+
				"then ReadFile/Stat/ReadDir/WalkDir), or add it here with the reason it cannot be.",
				pkg, n)
			continue
		}
		if n > want {
			t.Errorf("guards/%s: %d raw filesystem calls, allowed %d — a NEW one appeared. A gate "+
				"runs in the pre-commit path on a developer's machine; an unfenced read there is "+
				"unbounded by anything.", pkg, n, want)
		}
		if n < want {
			t.Errorf("guards/%s: %d raw filesystem calls but %d allowed — LOWER IT to %d in this "+
				"commit, so the paydown is banked instead of left as room to regrow", pkg, n, want, n)
		}
	}
	var gone []string
	for pkg := range allowedRawRead {
		if _, still := got[pkg]; !still {
			gone = append(gone, pkg)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		t.Errorf("these guards no longer read the filesystem directly — delete them from "+
			"allowedRawRead: %s", strings.Join(gone, ", "))
	}
}

// EVERY GATE MUST BUILD ITS READER FROM ITS OWN DECLARATION. The fence is only
// as good as where the reader comes from: a guard that constructs
// `capability.RepoAt(extension.Binding{Grants: …}, root)` inline has granted
// itself the capability, which is precisely the "grants annotate rather than
// constrain" state this whole layer exists to end.
//
// So the constructors that take a binding from a DECLARATION (RepoForGate,
// RepoContaining with a looked-up binding) are the sanctioned door, and a
// hand-built extension.Binding inside guards/ is not.
// ────────────────────────────────────────────────────────────────────────────
// IT CHECKED THE CALL AND NOW IT CHECKS THE LITERAL, WHICH IS THE ONLY VERSION
// THAT CAN HOLD.
//
// The rule used to be spelled as an alternation over the constructors that take a
// binding: `capability\.(RepoAt|KubeFor|CloudFor|For)\(\s*extension\.Binding\{`.
// An audit probed it against every shape a minted capability can take and it
// caught ONE of seven:
//
//	capability.For(extension.Binding{Grants: …})            caught
//	capability.RepoWriterAt(extension.Binding{Grants: …})   MISSED  ← a write handle
//	capability.RepoContainingWriter(…)                      MISSED  ← a write handle
//	capability.RepoContaining / RepoContainingAll(…)        MISSED
//	capability.WithExec(extension.Binding{Grants: …}, …)    MISSED
//	b := extension.Binding{Grants: …}; capability.For(b)    MISSED
//
// Twenty-four live call sites use constructors that were outside the alternation,
// and the two WRITE constructors were both among them. Worse than the coverage was
// the shape: an alternation has to be extended every time this package grows a
// constructor, and nothing makes anyone do it — the guard silently narrows as the
// thing it guards gets wider.
//
// THE LAST ROW IS THE ONE THAT SETTLES IT. Splitting the literal onto its own line
// defeats any pattern anchored on the CALL, however many constructors it lists.
// So the rule moved to the literal: a populated `extension.Binding{…}` outside a
// declaration is a capability someone granted themselves, wherever it is later
// passed. That is one rule instead of a list, it needs no maintenance when a
// constructor lands, and it is strictly wider than what it replaces.
//
// AN EMPTY LITERAL IS THE OPPOSITE OF A BYPASS AND MUST NOT MATCH.
// `capability.For(extension.Binding{})` grants NOTHING — it is the refusing
// default several Deps sets install so an un-installed seam cannot mutate a
// cluster. Six packages use it exactly that way. The dangerous shape is a
// POPULATED literal, so the check requires something inside the braces.
//
// ────────────────────────────────────────────────────────────────────────────
// AND THE EXEMPTION IS STRUCTURAL, BECAUSE BY-FILENAME WAS A HOLE YOU COULD PARK
// IN.
//
// The first version of this rule skipped `extension.go` and scanned everything
// else, on the reasoning that extension.go is where a declaration is SUPPOSED to
// live. A follow-up audit planted this in a real guard package's extension.go and
// the suite stayed green:
//
//	func sneakyBinding() extension.Binding {
//	    return extension.Binding{Kind: extension.Transition, State: extension.Seeded,
//	        Grants: []extension.Grant{extension.ClusterWrite, extension.SecretCustody}}
//	}
//
// A transition granting cluster-write and secret-custody, in no declaration, in
// the one file the guard agreed not to look at. And that file is the WORST one to
// exempt wholesale: all thirty-four binding accessors — seedBinding, laneBinding,
// repoBinding — already live there, so a constructed binding sits among a crowd of
// looked-up ones and reads exactly like them.
//
// So the exemption is no longer a path. A populated binding literal is legal only
// inside a function whose result type is `extension.Extension` — the declaration
// constructor itself, which is the thing `llz extension list` reads and Validate()
// judges. Every accessor returns `extension.Binding` and gets its value by
// ranging over `Extension().Bindings`, so all thirty-four stay legal and a
// thirty-fifth that CONSTRUCTS one does not.
//
// It is go/ast rather than a regex because the question is now about scope rather
// than text, and commands_census_test.go already established that shape next door:
// derive both sides, transcribe neither.
// ────────────────────────────────────────────────────────────────────────────

// mintedBindingsIn returns, for one parsed file, the position of every populated
// binding literal that is NOT inside a declaration constructor.
//
// It covers both spellings: `extension.Binding{…}` written out, and the elements
// of `[]extension.Binding{{…},{…}}`, whose types are elided and which would
// otherwise be invisible to a check keyed on the literal's own type.
func mintedBindingsIn(fset *token.FileSet, f *ast.File) []string {
	// The declaration constructors, by source range: a function whose result type
	// is `extension.Extension`. Anything inside one of these is the declaration.
	type span struct{ from, to token.Pos }
	var declarations []span
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Type.Results == nil {
			continue
		}
		for _, res := range fn.Type.Results.List {
			if isExtensionType(res.Type, "Extension") {
				declarations = append(declarations, span{fn.Pos(), fn.End()})
			}
		}
	}
	inDeclaration := func(p token.Pos) bool {
		for _, s := range declarations {
			if p >= s.from && p < s.to {
				return true
			}
		}
		return false
	}

	var out []string
	report := func(p token.Pos) {
		if !inDeclaration(p) {
			out = append(out, fset.Position(p).String())
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || lit.Type == nil {
			return true
		}
		switch {
		case isExtensionType(lit.Type, "Binding"):
			// `extension.Binding{}` is the refusing default and grants nothing.
			if len(lit.Elts) > 0 {
				report(lit.Pos())
			}
		case isBindingSlice(lit.Type):
			// The elements carry no type of their own, so they are judged here.
			for _, e := range lit.Elts {
				if el, ok := e.(*ast.CompositeLit); ok && len(el.Elts) > 0 {
					report(el.Pos())
				}
			}
		}
		return true
	})
	return out
}

// isExtensionType reports whether e is the selector `extension.<name>`.
func isExtensionType(e ast.Expr, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != name {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "extension"
}

// isBindingSlice reports whether e is `[]extension.Binding`.
func isBindingSlice(e ast.Expr) bool {
	arr, ok := e.(*ast.ArrayType)
	return ok && arr.Len == nil && isExtensionType(arr.Elt, "Binding")
}

// mintScanRoots is every tree that may name a binding.
//
// internal/cli JOINED THE WALK AFTER IT SHIPPED A BROKEN LANE. The rule had only
// ever covered internal/extensions, on the reasoning that an extension is what
// declares grants — which missed that the ASSEMBLY layer is the one that decides
// which declared binding a handle is built from, and is therefore the only place
// that can hand an extension somebody else's capability. It has no declaration
// constructor, so no binding literal there is legal, which is exactly right.
var mintScanRoots = []string{"../../extensions", "../../cli"}

func TestGuardsDoNotMintTheirOwnBindings(t *testing.T) {
	var scanned int
	fset := token.NewFileSet()
	walk := func(root string) error {
		return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parsing %s: %v", path, perr)
			}
			scanned++
			for _, at := range mintedBindingsIn(fset, f) {
				t.Errorf("%s builds an extension.Binding with fields set, outside any function that "+
					"returns an extension.Extension. Look the binding up from Extension() instead "+
					"(capability.RepoForGate, or an accessor like objenc's seedBinding or "+
					"reconcilelanes' laneBinding, both of which RANGE over Extension().Bindings) — a "+
					"capability minted at the call site has been through neither Validate() nor `llz "+
					"extension list`, so the reach a reviewer sees is not the reach the code has.", at)
			}
			return nil
		})
	}
	for _, root := range mintScanRoots {
		if err := walk(root); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	// A guard that walked an empty tree reports the same green as one that read
	// every file — the vacuity rule this package applies everywhere else.
	if scanned == 0 {
		t.Fatal("parsed no sources under " + strings.Join(mintScanRoots, " or ") +
			" — the trees moved and this guard has been vacuous since they did")
	}
	// The rule is only meaningful if declarations EXIST to be exempted; if none
	// parsed as such, every real declaration would be reported and someone would
	// weaken the rule rather than read it.
	if len(All62Declarations(t, "../../extensions", fset)) == 0 {
		t.Fatal("no function returning extension.Extension was found — the exemption has stopped " +
			"matching declarations, and the next run would flag all sixty-two of them")
	}
}

// A BINDING IS SELECTED BY NAME, NEVER BY POSITION.
//
// ────────────────────────────────────────────────────────────────────────────
// objenc WROTE THIS RULE DOWN AND NINE SITES BROKE IT ANYWAY.
//
// Its seedBinding comment: "`Bindings[0]` would be correct today and silently
// wrong the moment someone reorders the slice or adds a binding above it — and
// what it would be wrong ABOUT is which grants the custody handle is built from…
// capability.For would hand back refusing handles and the seeder would fail at the
// write with a permission message, sending the reader after a grant bug that does
// not exist."
//
// Every clause of that came true. internal/cli built assert-identity's and
// assert-secrets' Writers from `Extension().Bindings[0]`, an ASSERTION in both,
// while the transitions carrying cluster-write sat at index 1 and index 3. Both
// lanes run in `llz ci assert-suite`, both call the Writer for real, and both got
// a permission refusal — which reads as the capability layer working.
//
// A flagged anomaly is a defect report, not a footnote. So the rule is a rule now:
// Extension().MustBinding(name), or MustBindingOf(kind, state) where a declaration
// has one binding and no name to give it. Both panic on a miss, so a rename is
// loud instead of silently narrowing a capability.
// ────────────────────────────────────────────────────────────────────────────
func TestNoBindingIsSelectedByPosition(t *testing.T) {
	positional := regexp.MustCompile(`\.Bindings\[`)

	var scanned int
	for _, root := range mintScanRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			if positional.Match(stripComments(b)) {
				t.Errorf("%s selects a binding by POSITION. Which binding index 0 is depends on the "+
					"order someone typed the declaration in, and getting it wrong builds the handle "+
					"from the wrong grants — a refusing handle that fails later as a permission "+
					"error naming a grant nobody forgot. Use Extension().MustBinding(name), or "+
					"MustBindingOf(kind, state) when the declaration has a single unnamed binding.",
					filepath.ToSlash(path))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no sources — the trees moved and this guard has been vacuous since")
	}
	if !positional.MatchString(`Extension().Bindings[0]`) ||
		positional.MatchString(`Extension().MustBinding("drive")`) {
		t.Error("the positional-selection pattern no longer discriminates")
	}
}

// All62Declarations counts the declaration constructors the exemption recognises.
// Exported-looking name kept deliberately verbose: it is a corpus check, not an
// API.
func All62Declarations(t *testing.T, root string, fset *token.FileSet) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Type.Results == nil {
				continue
			}
			for _, res := range fn.Type.Results.List {
				if isExtensionType(res.Type, "Extension") {
					out = append(out, path+":"+fn.Name.Name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// THE RULE PASSES ON A CLEAN TREE BY CONSTRUCTION, so prove it discriminates
// against synthetic sources — every shape the regex it replaced was probed with
// and missed, plus the one the FILENAME exemption let through.
func TestTheMintedBindingDetectorFires(t *testing.T) {
	const preamble = "package x\n\n"
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"the shape the first rule caught",
			`func f() { h := capability.For(extension.Binding{Grants: g}) }`, true},
		{"a write handle the first rule missed",
			`func f() { w := capability.RepoWriterAt(extension.Binding{Grants: g}, root) }`, true},
		{"the other write handle",
			`func f() { w, _ := capability.RepoContainingWriter(extension.Binding{Grants: g}, p) }`, true},
		{"the test seam, which also takes a binding",
			`func f() { h := capability.WithExec(extension.Binding{Grants: g}, e, c) }`, true},
		{"the two-line form no call-anchored rule can see",
			"func f() {\n\tb := extension.Binding{Kind: extension.Transition}\n\th := capability.For(b)\n}", true},
		// THE ONE THE FILENAME EXEMPTION LET THROUGH. Transcribed from the probe
		// that found it: a constructed binding in a function returning a Binding,
		// sitting in extension.go among thirty-four accessors that look theirs up.
		{"a minted binding in an accessor, which extension.go used to hide",
			"func sneakyBinding() extension.Binding {\n\treturn extension.Binding{" +
				"Kind: extension.Transition, Grants: g}\n}", true},
		{"a package-level binding var, inside no function at all",
			`var sneaky = extension.Binding{Kind: extension.Transition, Grants: g}`, true},
		{"a slice of bindings outside a declaration",
			"func f() []extension.Binding {\n\treturn []extension.Binding{{Kind: extension.Gate}}\n}", true},

		{"the refusing empty default",
			`func f() { _ = capability.For(extension.Binding{}).Writer }`, false},
		{"a binding looked up from the declaration",
			`func f() { h := capability.For(seedBinding()) }`, false},
		{"an accessor that RANGES over the declaration",
			"func seedBinding() extension.Binding {\n\tfor _, b := range Extension().Bindings {\n\t\t" +
				"return b\n\t}\n\tpanic(\"none\")\n}", false},
		// The declaration itself, in both spellings, must be legal wherever it is.
		{"the declaration constructor",
			"func Extension() extension.Extension {\n\treturn extension.Extension{Name: \"x\",\n\t\t" +
				"Bindings: []extension.Binding{{Kind: extension.Gate, Grants: g}}}\n}", false},
		{"a second declaration constructor on one extension",
			"func PATExtension() extension.Extension {\n\treturn extension.Extension{\n\t\t" +
				"Bindings: []extension.Binding{{Kind: extension.Transition}}}\n}", false},
		{"an empty slice of bindings",
			`func f() { _ = []extension.Binding{} }`, false},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "probe.go", preamble+tc.src, 0)
		if err != nil {
			t.Fatalf("%s: parsing the probe source: %v", tc.name, err)
		}
		got := len(mintedBindingsIn(fset, f)) > 0
		if got != tc.want {
			t.Errorf("%s: flagged=%v want %v — src:\n%s", tc.name, got, tc.want, tc.src)
		}
	}
}

// A GUARD CAN LAUNDER A READ THROUGH A SHARED PACKAGE, and the direct-call
// ratchet above cannot see it. That is not hypothetical — it is exactly how
// pincoherence and templatemanifest stayed unfenced through the first pass while
// reporting zero raw calls, because their reads happened inside
// internal/shared/answers and internal/shared/manifest.
//
// So the unfenced entry points of those packages are named, and guards/ may not
// call them. The fenced twin of each takes a reader and is the only door a gate
// should use.
func TestGuardsDoNotReadThroughUnfencedHelpers(t *testing.T) {
	// call -> what to use instead.
	unfenced := map[string]string{
		"answers.Read(":           "answers.ReadFrom(repo, dir)",
		"manifest.Load(":          "manifest.LoadFrom(repo, root)",
		"manifest.Run(":           "manifest.RunFrom(repo, root, …)",
		"manifest.ScaffoldFiles(": "manifest.ScaffoldFilesFrom(repo, root)",
	}

	root := filepath.FromSlash("../../extensions/guards")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for call, instead := range unfenced {
				// The fenced twins are prefixes of nothing — ReadFrom does not
				// contain "Read(" — so a plain match is exact enough.
				if strings.Contains(line, call) {
					t.Errorf("%s calls %s, which reads the disk unfenced. A gate that launders its "+
						"reads through a shared package reports zero raw calls to the ratchet above "+
						"while being exactly as unbounded. Use %s.", path, call, instead)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
