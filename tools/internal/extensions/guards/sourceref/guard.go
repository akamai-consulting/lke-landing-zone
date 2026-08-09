package sourceref

// guard.go implements `llz ci source-ref-guard`: fail CI when a path literal
// naming the source tree points at something that does not exist.
//
// THE CHECK IS DELIBERATELY DUMB. It does not parse Markdown, YAML or Go — it
// extracts every `tools/...`-shaped string from the file's text and asks the
// filesystem. That is the whole point: the rot it exists to catch lives in prose,
// in `#` comments and in error-message string literals, none of which any
// structured parser reaches. A regex over raw text is the only reader that sees
// all three.
//
// WHY IT DOES NOT ACCEPT "HISTORICAL" PATHS. The obvious objection is that an ADR
// legitimately says "this used to live at <dead path>". It is a real case — this
// guard's own introduction had to rewrite one such sentence in ADR 0008. The
// answer is that a path literal in this repo is a POINTER, and a reader who meets
// one follows it; whether the author meant it as history is invisible at the
// point of use. History is expressible without a dead pointer ("in package main,
// since moved to versionpins.CITofuTag"), and that phrasing is strictly better
// for the reader, so the constraint costs nothing it should not cost.
//
// NO IGNORE-LIST, for the reason docs-guard's header gives about its own: an
// allowlist keyed on findings is a place to bury real breakage, and the only
// entries it would ever gain are the ones someone did not want to fix.

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// guardedPrefixes are the trees whose path literals are resolved.
//
// `tools/` is the tree under active decomposition — 60 extension packages and
// counting — so it is where a reference goes stale, and every one of the ~90
// rotted references that motivated this guard named it. `kubernetes-charts/` is
// here because it measured genuinely clean, and a ratchet is what keeps it that
// way; nothing was wrong with it.
//
// ────────────────────────────────────────────────────────────────────────────
// THE OTHER FIVE TREES ARE NOT HERE, AND "ADD A PREFIX" IS NOT WHAT IT COSTS.
//
// An earlier version of this comment claimed they "were scanned during
// development and are quiet". That was never measured — it was inferred, in a
// file whose whole subject is unverified claims about the repo. Measured, with
// trimRef's trailing-hyphen rule already applied:
//
//	docs 35    platform-apl 33    template-scripts 14
//	instance-template 6    terraform-modules 3    dockerfiles 3
//
// TWO CLASSES BLOCK THEM, and neither is answerable with an ignore-list — see
// this file's header for why that door stays shut:
//
//  1. RENDER-TIME ARTIFACTS. `docs/README.md` is referenced 21 times and does not
//     exist here BY DESIGN: `deliver-docs` writes it into a rendered instance,
//     and the docs skill says in as many words never to create one. Every one of
//     those references is correct. Shipping that prefix without modelling this
//     would tell 21 authors to fix working prose, which is how a guard earns its
//     deletion. docs-guard already carries the notion — renderTimeArtifact — so
//     the fix is to share it rather than reinvent it.
//
//  2. ILLUSTRATIVE PATHS. Nobody writes a `tools/`-shaped path to mean "some
//     file", so the class never appeared under this prefix. One tree over it is
//     everywhere: capability/repo.go explains what a scan root does to a
//     finding's name using invented platform-apl paths, and the plaintext guard's
//     tests do the same. They are examples, not claims, and no rule available
//     here separates the two.
//
// So the remaining five are a design slice, not a config change: solve (1) by
// sharing docs-guard's render-time set, and (2) by deciding whether an example
// path earns a convention — as `pkg's Symbol` did for the symbol half — or
// whether those trees are simply out of scope. Nothing else in this file is
// prefix-aware; the cost is entirely in those two answers.
var guardedPrefixes = []string{"tools", "kubernetes-charts"}

// refExpr matches a guarded path literal, with the character BEFORE it captured
// so the boundary rule below can judge it.
//
// The trailing character class is deliberately narrow: it stops at `<`, `{`, `(`,
// whitespace and quotes, so prose placeholders (`tools/internal/<pkg>`) and brace
// sets (`tools/internal/extensions/{a,b}/`) truncate to their real directory
// prefix and resolve, rather than being reported as paths they never were.
//
// THE LEADING BOUNDARY IS THE LOAD-BEARING PART, and it was not in the first
// draft — which reported 98 findings, of which a third were this. A path is only
// a path when it STARTS at `tools`; when something precedes it with a `/`, the
// match is the tail of a longer path that is not ours. Two shapes produced a
// third of that first draft's findings: a THIRD-PARTY module path with a `tools`
// segment (staticcheck is fetched from one, and it will never exist in this
// tree), and our OWN module import path, which the compiler already validates and
// which the guard has no business re-checking. Both vanish under one rule, and
// Go's regexp has no lookbehind, so the preceding byte is captured and tested
// instead. (Neither example is spelled out here as a literal — this guard reads
// its own source, and an illustration is indistinguishable from a claim.)
var refExpr = regexp.MustCompile(`(^|[^A-Za-z0-9_./-])((?:` +
	strings.Join(guardedPrefixes, "|") + `)/[A-Za-z0-9_.*/-]+)`)

// scanExts are the file types scanned. Makefile is matched by name, not suffix.
var scanExts = map[string]bool{".md": true, ".yml": true, ".yaml": true, ".sh": true, ".go": true}

// skipDir mirrors docs-guard's set and for the same reasons: generated trees,
// third-party caches, and a nested worktree of this same repo whose findings
// would be reported against the branch that is not it.
var skipDir = map[string]bool{
	".git": true, ".terraform": true, ".instance-test": true, ".e2e-instance": true,
	"node_modules": true, "vendor": true, "rendered": true, "bin": true, "worktrees": true,
}

// Finding is one path literal that did not resolve.
type Finding struct {
	File string
	Line int
	Ref  string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s does not exist", f.File, f.Line, f.Ref)
}

// Report is what a run examined and what it found. Scanned and Refs are exported
// because "how much did you look at" is the only thing that distinguishes a real
// green from a broken corpus, and the fail-closed arms below are asserted on them.
type Report struct {
	Scanned  int // files read
	Refs     int // path literals resolved
	Findings []Finding
}

// Run scans root and fails when any guarded path literal does not resolve.
func Run(root string) error {
	repo := capability.RepoForGate(Extension(), root)

	files, err := corpus(repo)
	if err != nil {
		return fmt.Errorf("source-ref-guard: %w", err)
	}

	var rep Report
	for _, rel := range files {
		data, err := repo.ReadFile(rel)
		if err != nil {
			// A file the guard could not read is a file it cannot vouch for, so the
			// run goes red naming it rather than reporting a coverage it does not
			// have. Same call docs-guard's loadDocs makes.
			return fmt.Errorf("source-ref-guard: %s could not be read (%w) — "+
				"the guard cannot vouch for a file it did not open", rel, err)
		}
		rep.Scanned++
		for _, r := range extractRefs(rel, string(data)) {
			rep.Refs++
			ok, err := resolves(repo, r.Ref)
			if err != nil {
				return fmt.Errorf("source-ref-guard: resolving %q from %s: %w", r.Ref, rel, err)
			}
			if !ok {
				rep.Findings = append(rep.Findings, r)
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
		fmt.Printf("::error file=%s,line=%d::%s no longer exists — update the reference "+
			"to where the code lives now, or rewrite the sentence so it names no path\n",
			f.File, f.Line, f.Ref)
	}
	if len(rep.Findings) > 0 {
		return fmt.Errorf("source-ref-guard: %d stale path reference(s) across %d file(s)",
			len(rep.Findings), countFiles(rep.Findings))
	}

	// FAIL CLOSED ON BOTH AXES. A guard that examined nothing reports the same
	// green as one that examined everything, and this guard has two distinct ways
	// to examine nothing: no files (a wrong --root, the classic) and no references
	// (a corpus found but the extraction broken, which is what a bad edit to
	// refExpr looks like). The second is the one a test would not have caught —
	// every unit test here supplies its own body, so a regex that matches nothing
	// in the real tree still passes them.
	if rep.Scanned == 0 {
		return fmt.Errorf("source-ref-guard: no scannable file under %q — a guard that "+
			"examined nothing cannot report that everything is fine. Check --root", root)
	}
	if rep.Refs == 0 {
		return fmt.Errorf("source-ref-guard: %d file(s) scanned but not one %s/ path "+
			"literal found — this repo documents itself constantly, so zero means the "+
			"extraction is broken, not that the docs are path-free",
			rep.Scanned, strings.Join(guardedPrefixes, "/, "))
	}

	fmt.Printf("source-ref-guard: %d %s/ path reference(s) across %d file(s) all resolve.\n",
		rep.Refs, strings.Join(guardedPrefixes, "/, "), rep.Scanned)
	return nil
}

func countFiles(fs []Finding) int {
	seen := map[string]bool{}
	for _, f := range fs {
		seen[f.File] = true
	}
	return len(seen)
}

// corpus returns every scannable file, repo-relative and sorted.
func corpus(repo capability.Repo) ([]string, error) {
	var out []string
	err := repo.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		// FAIL, don't skip — swallowing a walk error drops part of the tree while
		// the run still prints "N file(s)", which is the false green this guard is.
		if err != nil {
			return fmt.Errorf("walk %s: %w", p, err)
		}
		if d.IsDir() {
			// Never skip the root itself: it arrives as "." or "..", whose basename
			// would match a name rule and skip the whole walk.
			if p == "." {
				return nil
			}
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if scannable(d.Name()) {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// scannable reports whether a file's text is scanned.
//
// _test.go IS EXCLUDED, AND IT IS A FILE CLASS RATHER THAN AN IGNORE-LIST. A
// test that proves this guard reports a broken path must CONTAIN a broken path;
// so must every table entry exercising the negative arm of anything else that
// handles paths. Scanning them would make the guard fail on its own fixtures and
// the only available fix would be per-finding suppression — the thing this file's
// header refuses. The exclusion is structural, states its reason, and cannot grow.
func scannable(name string) bool {
	if strings.HasSuffix(name, "_test.go") {
		return false
	}
	if name == "Makefile" {
		return true
	}
	return scanExts[path.Ext(name)]
}

// extractRefs returns every guarded path literal in body with its 1-indexed line.
// Pure over its input so the interesting cases are unit-testable without a tree.
func extractRefs(file, body string) []Finding {
	var out []Finding
	for i, line := range strings.Split(body, "\n") {
		for _, m := range refExpr.FindAllStringSubmatchIndex(line, -1) {
			// Group 2 is the path; group 1 was only the boundary the regex had to
			// consume in place of a lookbehind.
			start, end := m[4], m[5]
			if isRegexFragment(line, start, end) {
				continue
			}
			p := trimRef(line[start:end])
			if p == "" || strings.Contains(p, "**") {
				// `**` needs a recursive match this guard has no reason to implement:
				// the only literals carrying one are glob PATTERNS in prose
				// ("tools/internal/**/*_test.go"), which name a shape rather than a
				// place. Skipped rather than approximated.
				continue
			}
			out = append(out, Finding{File: file, Line: i + 1, Ref: p})
		}
	}
	return out
}

// isRegexFragment reports whether a match is part of a REGULAR EXPRESSION rather
// than a path. The Makefile's change-aware `lint` recipe is built from anchored
// grep -E alternations over changed files, and each branch of one truncates into
// something path-shaped but meaningless — a directory followed by a wildcard, or
// a name cut off at the escape before its extension. Both were reported by the
// first draft. Two signals separate them from real paths, and both are things a
// filesystem path in this repo cannot contain:
//
//   - `.*` — a regex wildcard. A glob writes `*.go`, never `/.*`.
//   - a trailing `\` — the escape before `\.`, which is how a regex spells a
//     literal dot and how the match got truncated mid-path in the first place.
func isRegexFragment(line string, start, end int) bool {
	if strings.Contains(line[start:end], ".*") {
		return true
	}
	return end < len(line) && line[end] == '\\'
}

// trimRef strips the trailing punctuation a path collects from prose — the `.`
// ending a sentence and the `/` ending a directory. Neither is legal as the last
// character of a file this repo contains, and `.go` ends in `o`, so nothing real
// is damaged.
//
// A TRAILING `-` IS NOT TRIMMED, IT DISQUALIFIES. An earlier draft trimmed it
// alongside the other two, which was fine for `the tools/cmd/llz/ tree-` and
// wrong for every long hyphenated filename a paragraph wraps mid-word:
// `docs/designs/team-scoped-credentials.md` broken across lines yields
// `docs/designs/team-scoped-`, and trimming turned that into a confident claim
// about a file nobody wrote. Returning "" skips it instead. A wrapped path is
// genuinely unverifiable from one line, and the honest response to unverifiable
// is to say nothing rather than to invent the half that is missing.
func trimRef(s string) string {
	if strings.HasSuffix(s, "-") {
		return ""
	}
	return strings.TrimRight(s, "./")
}

// resolves reports whether a path literal names something in the tree. A literal
// containing `*` is a PATTERN and must match at least one entry — a glob that
// matches nothing is the same defect as a path that does not exist, and the
// budget gate's own header records a one-character typo (`llz` → `IIz`) that
// retired a gate permanently by exactly this route while it kept reporting green.
func resolves(repo capability.Repo, ref string) (bool, error) {
	if !strings.Contains(ref, "*") {
		if _, err := repo.Stat(ref); err != nil {
			return false, nil
		}
		return true, nil
	}
	dir, pattern := path.Split(ref)
	if strings.Contains(dir, "*") {
		// A wildcard in a DIRECTORY segment would need the same recursive match
		// `**` does. Treat the directory prefix as the claim instead: it is the
		// part a reader navigates to, and it is checkable.
		return resolves(repo, strings.TrimSuffix(dir, "/"))
	}
	entries, err := repo.ReadDir(strings.TrimSuffix(dir, "/"))
	if err != nil {
		return false, nil // the directory itself is gone — a finding, not an error
	}
	for _, e := range entries {
		ok, err := path.Match(pattern, e.Name())
		if err != nil {
			return false, fmt.Errorf("bad pattern %q: %w", pattern, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
