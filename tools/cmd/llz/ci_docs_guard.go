package main

// ci_docs_guard.go — `llz ci docs-guard`: fail CI when the docs describe a CLI,
// a workflow, or a repo layout that no longer exists.
//
// WHY THIS EXISTS. A full audit of the 104 Markdown files found 30 defects, and
// the large majority were mechanically detectable from the repo itself — a flag
// that had been renamed, a `gh workflow run` naming inputs the workflow never
// declared, a relative link to a file that had moved. None of them were subtle.
// They survived because nothing ever asked.
//
// The costly ones all shared a shape: a doc that was TRUE when written and went
// stale silently when the code moved, in a file nobody re-reads because it is only
// opened during an incident. So the guard checks the docs against the running
// binary and the workflow YAML, not against a hand-maintained list that would rot
// the same way.
//
// THREE CHECKS, in ascending order of what they cost to get wrong:
//
//  1. COMMANDS — every `llz …` invocation resolves to a real command, and every
//     `--flag` on it is one that command accepts. Walks the live cobra tree, so it
//     cannot drift from the CLI.
//
//  2. WORKFLOW DISPATCHES — every `gh workflow run <wf> -f k=v` names a workflow
//     that exists and inputs it declares. `gh` rejects an undeclared input, so
//     these fail at the worst time: an operator following a runbook step by step.
//
//  3. LINKS — every relative Markdown link resolves, evaluated BOTH in the
//     template tree and against the post-`deliver-docs` keep-set, because the
//     delivered operator docs are the ones every adopter carries and are exactly
//     where the audit found the rot concentrated.
//
// DELIBERATELY NOT CHECKED: prose claims about behaviour ("multi-tenancy is off"),
// which is what the audit's worst findings actually were. No linter catches those.
// The guard buys back the mechanical half so review attention can go to the rest.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// docsScanned counts what each check actually EXAMINED, not what it was handed.
//
// WHY A COUNTER. Every defect this guard has had was the same shape: it scanned
// LESS than it claimed and still printed a clean result — a walk that skipped the
// root, a dot-rule that dropped .github/, a parser that stopped at the first flag,
// at a version string, at `<env>`. None of them changed the file count, so none
// were visible. Reporting what was examined turns the next one into a number that
// moves, and the repo-level test asserts FLOORS on it — so a clean run over a
// shrunken scan fails instead of reassuring.
type docsScanned struct {
	invocations int // `llz …` commands parsed
	flags       int // flags actually VALIDATED against a resolved command
	dispatches  int // `gh workflow run` calls parsed
	links       int // relative links resolved
}

func (c docsScanned) String() string {
	return fmt.Sprintf("%d llz invocation(s) / %d flag(s), %d workflow dispatch(es), %d link(s)",
		c.invocations, c.flags, c.dispatches, c.links)
}

type docFinding struct {
	File, Detail string
	Line         int
	Kind         string
}

func (f docFinding) String() string {
	return fmt.Sprintf("%s:%d: [%s] %s", f.File, f.Line, f.Kind, f.Detail)
}

func ciDocsGuardCmd() *cobra.Command {
	var root string
	var skipLinks, skipCommands, skipWorkflows bool
	c := &cobra.Command{
		Use:   "docs-guard",
		Short: "fail when the docs name a command, flag, workflow input or path that does not exist",
		Long: "Validates every Markdown file against the repo it documents:\n" +
			"  • every `llz …` invocation + its flags, against the live cobra tree\n" +
			"  • every `gh workflow run` input, against the workflow's declared inputs\n" +
			"  • every relative link, in the template tree AND in the delivered\n" +
			"    (post-`deliver-docs`) operator set\n" +
			"Reports every finding, then exits 1. Catches the mechanical half of doc rot;\n" +
			"prose claims about behaviour still need a human.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			files, err := markdownFiles(root)
			if err != nil {
				return err
			}
			// Read once. An unreadable file becomes a FINDING here rather than a
			// silent skip in each of the three checks below — a guard that cannot
			// read a doc must not report that it checked it.
			docs, findings := loadDocs(root, files)
			var n docsScanned
			if !skipCommands {
				findings = append(findings, checkDocCommands(docs, cmd.Root(), &n)...)
			}
			if !skipWorkflows {
				f, err := checkDocWorkflowInputs(root, docs, &n)
				if err != nil {
					return err
				}
				findings = append(findings, f...)
			}
			if !skipLinks {
				findings = append(findings, checkDocLinks(root, docs, &n)...)
			}
			sort.Slice(findings, func(i, j int) bool {
				if findings[i].File != findings[j].File {
					return findings[i].File < findings[j].File
				}
				return findings[i].Line < findings[j].Line
			})
			for _, f := range findings {
				fmt.Println(f)
			}
			if len(findings) > 0 {
				fmt.Printf("docs-guard: checked %s across %d file(s).\n", n, len(docs))
				return fmt.Errorf("docs-guard: %d finding(s) across %d Markdown file(s)", len(findings), len(files))
			}
			fmt.Printf("docs-guard: %d Markdown file(s) OK — checked %s.\n", len(docs), n)
			return nil
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	c.Flags().BoolVar(&skipCommands, "skip-commands", false, "skip the llz command/flag check")
	c.Flags().BoolVar(&skipWorkflows, "skip-workflows", false, "skip the gh workflow run input check")
	c.Flags().BoolVar(&skipLinks, "skip-links", false, "skip the relative-link check")
	return c
}

// docsGuardSkipDir names the directories that hold Markdown belonging to
// something other than this repo — build output and vendored third-party trees,
// whose links resolve against THEIR project and would report as dead here.
//
// It is an explicit DENY-set, not a "skip every dot-directory" rule. That rule
// was tried and silently dropped four real docs — `.github/PULL_REQUEST_TEMPLATE.md`,
// the two issue templates, and `.github/workflows/AGENTS.md` — taking the guard
// from 105 files to 101 while still reporting success. A deny-set fails in the
// safe direction: an artifact tree nobody listed produces noisy findings, which
// gets fixed. An over-broad skip produces silent under-coverage, which does not.
// renderTimeArtifact names paths that exist in a RENDERED instance but not in the
// template, because the render itself creates them. They are not dead links — the
// guard simply cannot see them from here. Keep this list tiny and cite the creator,
// so it stays a statement of fact rather than a place to bury real breakage.
var renderTimeArtifact = map[string]bool{
	// runDeliverDocs writes it (docsPointer) after pruning docs/ to the keep-set.
	"docs/README.md": true,
}

// The scaffold subtree, whose Markdown renders to the instance ROOT.
const (
	scaffoldDirName = "instance-template"
	scaffoldPrefix  = scaffoldDirName + "/"
)

var docsGuardSkipDir = map[string]bool{
	".git":           true,
	".terraform":     true, // provider/module cache: third-party READMEs
	".instance-test": true, // the rendered instance `make instance-test` leaves
	".e2e-instance":  true, // the hoisted instance e2e-instantiate builds
	"node_modules":   true,
	"vendor":         true,
	"rendered":       true, // `make render-charts` output
	"bin":            true,
}

func markdownFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		// FAIL, don't skip. Swallowing a walk error (a permission bit, a broken
		// symlink) drops part of the tree while the run still prints "N file(s)
		// OK" — a false green, which is the exact defect this guard exists to
		// catch. Under-covering silently is worse than not running.
		if err != nil {
			return fmt.Errorf("walk %s: %w", p, err)
		}
		if d.IsDir() {
			// NEVER skip the root itself — it is routinely passed as "." or "..",
			// whose basename would match a name-based rule and skip the entire
			// walk, reporting a clean "0 files OK" while checking nothing.
			if p == root {
				return nil
			}
			if docsGuardSkipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".md") {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// docFile is a Markdown file already read into memory. The four checks below
// used to each os.ReadFile the same path and `continue` on error — four silent
// skips per unreadable file, and four reads per file in the happy path. Reading
// once, here, makes an I/O failure a FINDING (the run goes red and names the
// file) instead of invisible under-coverage.
type docFile struct {
	rel  string
	body string
}

// loadDocs reads every file, returning the readable ones and a finding per
// failure. It never returns a partial set silently.
func loadDocs(root string, files []string) ([]docFile, []docFinding) {
	docs := make([]docFile, 0, len(files))
	var bad []docFinding
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			bad = append(bad, docFinding{
				File: rel, Line: 0, Kind: "unreadable",
				Detail: fmt.Sprintf("could not be read (%v) — the guard cannot vouch for this file, so the run fails rather than reporting a coverage it does not have", err),
			})
			continue
		}
		docs = append(docs, docFile{rel: rel, body: string(data)})
	}
	return docs, bad
}

// ── 1. llz commands + flags ──────────────────────────────────────────────────

// llzStartRe finds where an `llz` invocation BEGINS. It deliberately does not
// try to match the whole command: an earlier version did, and its alternation
// stopped at the first token it could not classify — so a value like
// `v1.33.6+lke7` or `203.0.113.0/24` ended the match and every flag after it went
// unchecked, on one line or many. Find the start, then TOKENISE the rest.
var llzStartRe = regexp.MustCompile(`(?:^|[^\w./-])llz(\s+.*)$`)

// invocationTokens splits the text after `llz` into argv-ish tokens, stopping at
// whatever ends the command: a shell operator, a comment, or the closing backtick
// of inline code. Quoted strings are kept whole so a value containing a space is
// one token.
func invocationTokens(rest string) []string {
	var out []string
	var cur strings.Builder
	quote := rune(0)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	runes := []rune(rest)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case ' ', '\t':
			flush()
		case '`':
			// Always ends it: this is the closing backtick of markdown inline
			// code, and it butts straight up against the last token.
			flush()
			return out
		case '<':
			// NEVER a terminator. In these docs `<` opens a placeholder — `<env>`,
			// `<owner>/<name>` — 92 times across the corpus, and it sits exactly
			// where a token starts. An earlier cut terminated on it to catch input
			// redirects (which appear nowhere here), so the scan stopped at the
			// placeholder and skipped every flag after it on ~92 invocations.
			cur.WriteRune(c)
		case '|', '#', ';', '&', '>', ')':
			// Shell operators ONLY at a token boundary. Mid-token they are
			// ordinary characters — notably the `>` that CLOSES a placeholder.
			if cur.Len() > 0 {
				cur.WriteRune(c)
				continue
			}
			flush()
			return out // a real operator: `> file`, `| jq`, `# comment`
		case '\\':
			flush() // a stray continuation marker; tokens continue on the folded line
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}

// logicalLine is a source line after shell CONTINUATIONS have been folded in,
// carrying the line number of where it STARTED so a finding still points at the
// place a reader will look.
type logicalLine struct {
	text string
	num  int
}

// foldContinuations joins `\`-continued lines into one logical line. The command
// scan is line-based and llzInvocationRe stops at a newline, so without this a
// multi-line invocation had only its FIRST line checked — and multi-line is
// exactly the shape of the copy/paste blocks most likely to drift. quickstart.md's
// flagship `llz env add … \` block spans three lines; two of them were unguarded.
func foldContinuations(body string) []logicalLine {
	raw := strings.Split(body, "\n")
	out := make([]logicalLine, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		start, cur := i+1, raw[i]
		for strings.HasSuffix(strings.TrimRight(cur, " \t"), "\\") && i+1 < len(raw) {
			cur = strings.TrimSuffix(strings.TrimRight(cur, " \t"), "\\") + " " + strings.TrimSpace(raw[i+1])
			i++
		}
		out = append(out, logicalLine{text: cur, num: start})
	}
	return out
}

// isDecisionRecord reports whether a doc is a dated RECORD rather than an
// instruction. ADRs and design docs describe what was decided (and often what was
// rejected, or proposed and never built) at a moment in time — an ADR that names a
// flag which no longer exists is doing its job, and rewriting one to match today's
// CLI would falsify the record. So they are exempt from the command/flag check and
// still subject to the link check, which is about navigation rather than accuracy.
//
// Everything else — quickstart, adopter guide, runbooks, playbooks, chart and
// module READMEs — tells a reader to run something, and is checked.
func isDecisionRecord(rel string) bool {
	rel = filepath.ToSlash(rel)
	return strings.HasPrefix(rel, "docs/adr/") || strings.HasPrefix(rel, "docs/designs/")
}

// flagTakesValue reports whether --name consumes the NEXT token as its value.
// Bool flags do not, which is what lets `llz --yes ci …` still resolve `ci`
// rather than swallowing it as a value. An UNKNOWN flag is assumed value-less on
// purpose: guessing "it takes a value" would swallow a real subcommand and hide
// the very drift this guard exists to find, whereas guessing wrong the other way
// costs at most one spurious positional that the descent ignores.
func flagTakesValue(cur, root *cobra.Command, name string) bool {
	n := strings.TrimLeft(name, "-")
	f := cur.Flags().Lookup(n)
	if f == nil {
		f = cur.InheritedFlags().Lookup(n)
	}
	if f == nil {
		f = root.PersistentFlags().Lookup(n)
	}
	return f != nil && f.Value.Type() != "bool"
}

// Prose regularly reads "the llz binary", "an llz workload", "the llz image".
// Those parse as a subcommand and are not one; the check only reports a token
// run whose FIRST word is a real top-level command, so ordinary English is
// invisible to it rather than needing an ignore list.
func checkDocCommands(docs []docFile, rootCmd *cobra.Command, n *docsScanned) []docFinding {
	var out []docFinding
	for _, d := range docs {
		rel := d.rel
		if isDecisionRecord(rel) {
			continue
		}
		for _, ll := range foldContinuations(d.body) {
			for _, m := range llzStartRe.FindAllStringSubmatch(ll.text, -1) {
				n.invocations++
				// Resolve the command and its flags in ONE pass, the way cobra
				// itself accepts them: flags may appear before, between, or after
				// subcommand words. An earlier cut stopped collecting words at the
				// first flag and treated everything after it as a flag value —
				// which made `llz --yes ci reap-volumes --bogus` resolve to NO
				// command at all and skip silently. That is a doc style this repo
				// uses (orphan-volume-cleanup.md), so the guard was blind exactly
				// where it claimed coverage.
				fields := invocationTokens(m[1])
				cur, words, flags, descending := rootCmd, 0, []string(nil), true
				for i := 0; i < len(fields); i++ {
					f := fields[i]
					if f == "--" {
						// The argv separator (`llz openbao exec -- kv get …`).
						// Everything after it belongs to the inner command.
						break
					}
					if strings.HasPrefix(f, "-") {
						name := strings.SplitN(f, "=", 2)[0]
						flags = append(flags, name)
						// A VALUE-taking flag consumes the next token, so it must
						// not be mistaken for a subcommand. A bool flag does not —
						// which is the whole point: `--yes ci` must still find `ci`.
						if !strings.Contains(f, "=") && flagTakesValue(cur, rootCmd, name) {
							i++
						}
						continue
					}
					if descending {
						if next, _, err := cur.Find([]string{f}); err == nil && next != cur {
							cur, words = next, words+1
							continue
						}
						descending = false // a positional; stop descending
					}
				}
				if words == 0 {
					continue // no command resolved — prose, or a bare `llz`
				}
				for _, fl := range flags {
					if fl == "--help" || fl == "-h" || !strings.HasPrefix(fl, "--") {
						continue
					}
					// Counted HERE, not per invocation: a truncating parser still
					// finds the invocation, it just stops collecting. Invocation
					// count therefore does NOT move when the scan is blinded —
					// measured, not assumed — so the flag count is the metric.
					n.flags++
					name := strings.TrimPrefix(fl, "--")
					path := strings.TrimPrefix(cur.CommandPath(), "llz ")
					found := cur.Flags().Lookup(name)
					if found == nil {
						found = cur.InheritedFlags().Lookup(name)
					}
					if found == nil {
						found = rootCmd.PersistentFlags().Lookup(name)
					}
					if found == nil {
						out = append(out, docFinding{
							File: rel, Line: ll.num, Kind: "flag",
							Detail: fmt.Sprintf("`llz %s` has no flag %s", path, fl),
						})
						continue
					}
					// A DEPRECATED flag still works, so nothing fails — which is
					// exactly why docs keep teaching one long after the rename.
					// It is a finding, not a hard error, and the deprecation
					// message already carries the replacement.
					if found.Deprecated != "" {
						out = append(out, docFinding{
							File: rel, Line: ll.num, Kind: "deprecated-flag",
							Detail: fmt.Sprintf("`llz %s %s` is deprecated: %s", path, fl, found.Deprecated),
						})
					}
				}
			}
		}
	}
	return out
}

// ── 2. gh workflow run inputs ────────────────────────────────────────────────

var ghRunRe = regexp.MustCompile(`gh workflow run\s+([\w.-]+\.ya?ml)((?:[^\n]*\\\n)*[^\n]*)`)
var ghFieldRe = regexp.MustCompile(`(?:--field|-f)\s+([A-Za-z_][\w-]*)=`)

type wfInputs struct {
	names    map[string]bool
	required map[string]bool
	dispatch bool
}

func checkDocWorkflowInputs(root string, docs []docFile, n *docsScanned) ([]docFinding, error) {
	wfs, err := loadWorkflowInputs(root)
	if err != nil {
		return nil, err
	}
	if len(wfs) == 0 {
		return nil, nil
	}
	var out []docFinding
	for _, d := range docs {
		rel, text := d.rel, d.body
		for _, m := range ghRunRe.FindAllStringSubmatchIndex(text, -1) {
			whole := text[m[0]:m[1]]
			sub := ghRunRe.FindStringSubmatch(whole)
			name, rest := sub[1], sub[2]
			line := strings.Count(text[:m[0]], "\n") + 1
			n.dispatches++
			wf, ok := wfs[name]
			if !ok {
				out = append(out, docFinding{File: rel, Line: line, Kind: "workflow",
					Detail: fmt.Sprintf("no workflow named %s in this repo", name)})
				continue
			}
			if !wf.dispatch {
				out = append(out, docFinding{File: rel, Line: line, Kind: "workflow",
					Detail: fmt.Sprintf("%s has no workflow_dispatch trigger — it cannot be run this way", name)})
				continue
			}
			used := map[string]bool{}
			for _, f := range ghFieldRe.FindAllStringSubmatch(rest, -1) {
				used[f[1]] = true
			}
			var unknown, missing []string
			for u := range used {
				if !wf.names[u] {
					unknown = append(unknown, u)
				}
			}
			for r := range wf.required {
				if !used[r] {
					missing = append(missing, r)
				}
			}
			sort.Strings(unknown)
			sort.Strings(missing)
			if len(unknown) > 0 {
				out = append(out, docFinding{File: rel, Line: line, Kind: "workflow-input",
					Detail: fmt.Sprintf("%s does not declare input(s) %s — gh rejects undeclared inputs",
						name, strings.Join(unknown, ", "))})
			}
			if len(missing) > 0 {
				out = append(out, docFinding{File: rel, Line: line, Kind: "workflow-input",
					Detail: fmt.Sprintf("%s requires input(s) %s, not given", name, strings.Join(missing, ", "))})
			}
		}
	}
	return out, nil
}

// loadWorkflowInputs reads workflow_dispatch inputs out of every workflow in the
// repo, keyed by BASENAME — which is how a doc names one (`gh workflow run
// terraform.yml`). A basename collision between the template's own workflows and
// the instance-template's is resolved toward the instance, since that is the repo
// an operator dispatches against.
func loadWorkflowInputs(root string) (map[string]*wfInputs, error) {
	out := map[string]*wfInputs{}
	dirs := []string{
		filepath.Join(root, ".github", "workflows"),
		filepath.Join(root, "instance-template", ".github", "workflows"),
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // a repo without one of these is fine
			}
			// Anything else (a permission bit) would silently drop every
			// workflow in that tree and let bad dispatch inputs through.
			return nil, fmt.Errorf("read workflow dir %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			wi, err := parseWorkflowDispatchInputs(data)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", filepath.Join(dir, e.Name()), err)
			}
			out[e.Name()] = wi
		}
	}
	return out, nil
}

// parseWorkflowDispatchInputs pulls the workflow_dispatch input names (and which
// are required) out of a workflow. Pure over the bytes, so the YAML shapes GitHub
// allows — `on:` as a map, and the notorious `on:` parsing as the boolean true —
// are covered by unit tests rather than discovered in CI.
func parseWorkflowDispatchInputs(data []byte) (*wfInputs, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	wi := &wfInputs{names: map[string]bool{}, required: map[string]bool{}}
	// YAML 1.1 folds an unquoted `on` key to the boolean true; go-yaml v3 keeps
	// it a string, but a workflow may also quote it. Accept every spelling.
	var on any
	for _, k := range []any{"on", true, "'on'", `"on"`} {
		if v, ok := doc[fmt.Sprint(k)]; ok {
			on = v
			break
		}
	}
	if on == nil {
		if v, ok := doc["true"]; ok {
			on = v
		}
	}
	onMap, ok := on.(map[string]any)
	if !ok {
		return wi, nil // `on: [push]` or a bare string — no dispatch inputs
	}
	wd, ok := onMap["workflow_dispatch"]
	if !ok {
		return wi, nil
	}
	wi.dispatch = true
	wdMap, ok := wd.(map[string]any)
	if !ok {
		return wi, nil // `workflow_dispatch:` with no inputs
	}
	inputs, ok := wdMap["inputs"].(map[string]any)
	if !ok {
		return wi, nil
	}
	for name, spec := range inputs {
		wi.names[name] = true
		if sm, ok := spec.(map[string]any); ok {
			if req, ok := sm["required"].(bool); ok && req {
				// An input with a default is satisfiable without being passed,
				// so it is not something a doc MUST name.
				if _, hasDefault := sm["default"]; !hasDefault {
					wi.required[name] = true
				}
			}
		}
	}
	return wi, nil
}

// ── 3. relative links, in the template AND after delivery ────────────────────

var docsGuardLinkRe = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

func checkDocLinks(root string, docs []docFile, n *docsScanned) []docFinding {
	var out []docFinding
	for _, d := range docs {
		rel := d.rel
		dir := filepath.Dir(rel)
		// A file under instance-template/ RENDERS to the instance root, so its
		// links must be judged as they will appear there — not skipped, which is
		// what an earlier cut did on the grounds that they "resolve in a rendered
		// instance". Nothing judged them in a rendered instance either, so the
		// links that actually shipped dead to adopters (AGENTS.md ->
		// docs/adopter-guide.md) were in the one tree the guard ignored.
		//
		// After render, `instance-template/X.md` is `X.md` and a target resolves
		// against that. It is satisfied by EITHER the repo root (docs/ lives there
		// and is copied in at render) or instance-template/ (template-owned files
		// that render alongside).
		instRel := strings.TrimPrefix(filepath.ToSlash(rel), scaffoldPrefix)
		rendered := rel != instRel
		linkDir := dir
		if rendered {
			linkDir = filepath.Dir(instRel)
			if linkDir == "." {
				linkDir = ""
			}
		}
		for i, line := range strings.Split(d.body, "\n") {
			for _, m := range docsGuardLinkRe.FindAllStringSubmatch(line, -1) {
				target := m[1]
				if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
					strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				path := strings.SplitN(target, "#", 2)[0]
				if path == "" {
					continue
				}
				n.links++
				resolved := filepath.Clean(filepath.Join(linkDir, path))
				// A rendered instance has NOTHING above its root, so a link that
				// climbs past it is dead there however it resolves here. Catch it
				// before the existence probes: filepath.Join(root, "../x") walks
				// out of the repo and can land on a same-named directory by pure
				// coincidence — which is how `../../platform-apl/` from
				// apl-values/README.md passed while being dead in every instance.
				if rendered && strings.HasPrefix(resolved, "..") {
					out = append(out, docFinding{File: rel, Line: i + 1, Kind: "link",
						Detail: fmt.Sprintf("%s climbs above the instance root — dead in a rendered instance; use an absolute template URL", target)})
					continue
				}
				if pathExists(filepath.Join(root, resolved)) {
					continue
				}
				if rendered && pathExists(filepath.Join(root, scaffoldDirName, resolved)) {
					continue // template-owned; renders into the instance beside this file
				}
				if rendered && renderTimeArtifact[filepath.ToSlash(resolved)] {
					continue // written during render, so absent here by construction
				}
				out = append(out, docFinding{File: rel, Line: i + 1, Kind: "link",
					Detail: fmt.Sprintf("%s does not exist", target)})
			}
		}
	}
	out = append(out, checkDeliveredDocLinks(root, docs, n)...)
	return out
}

// checkDeliveredDocLinks is the check that matters most: it evaluates the kept
// docs against the keep-set they will actually ship with. A runbook that links a
// sibling runbook is fine; one that links a doc `deliver-docs` prunes is fine too
// (the rewrite repoints it) — but only if the rewrite can SEE it, which is what
// the audit found it could not do from the instance root.
func checkDeliveredDocLinks(root string, docs []docFile, n *docsScanned) []docFinding {
	var out []docFinding
	delivered := func(rel string) bool {
		if !strings.HasPrefix(rel, "docs/") {
			return false
		}
		top := strings.SplitN(strings.TrimPrefix(rel, "docs/"), "/", 2)[0]
		return docsKeep[top]
	}
	for _, d := range docs {
		rel := d.rel
		if !delivered(rel) {
			continue
		}
		dir := filepath.Dir(rel)
		for i, line := range strings.Split(d.body, "\n") {
			for _, m := range docsGuardLinkRe.FindAllStringSubmatch(line, -1) {
				target := m[1]
				if strings.HasPrefix(target, "http") || strings.HasPrefix(target, "#") ||
					strings.HasPrefix(target, "mailto:") {
					continue
				}
				path := strings.SplitN(target, "#", 2)[0]
				if path == "" {
					continue
				}
				resolved := filepath.Clean(filepath.Join(dir, path))
				// A link OUT of docs/ cannot be repointed by deliver-docs (its
				// rewrite is docs/-relative) and will not resolve in an instance.
				if !strings.HasPrefix(resolved, "docs/") {
					out = append(out, docFinding{File: rel, Line: i + 1, Kind: "delivered-link",
						Detail: fmt.Sprintf("%s escapes docs/ — dead in a delivered instance; use an absolute template URL", target)})
					continue
				}
				// Inside docs/: either still delivered (relative link works) or
				// pruned (deliver-docs repoints it). Only a target that exists in
				// NEITHER is broken.
				if !pathExists(filepath.Join(root, resolved)) {
					out = append(out, docFinding{File: rel, Line: i + 1, Kind: "delivered-link",
						Detail: fmt.Sprintf("%s does not exist", target)})
				}
			}
		}
	}
	return out
}
