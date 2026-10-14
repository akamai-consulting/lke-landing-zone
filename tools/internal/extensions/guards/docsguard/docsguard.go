package docsguard

// docsguard.go — `llz ci docs-guard`: fail CI when the docs describe a CLI,
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
// FOUR CHECKS, in ascending order of what they cost to get wrong:
//
//  1. FLAGS — for every `llz …` invocation whose command RESOLVES, each `--flag`
//     is one that command accepts (deprecated ones are reported too). Walks the
//     live cobra tree, so it cannot drift from the CLI.
//
//     It does NOT report an unknown top-level command. Measured before deciding:
//     the only doc lines starting `llz <word>` where <word> is not in the tree are
//     `llz smoke` and `llz psql` in extending-llz.md — USER-DEFINED commands from
//     .llz/commands.yaml, which by design never appear in the binary. Reporting
//     unknown commands would flag the doc that teaches the extension mechanism,
//     and the only fix would be an ignore-list — a place to bury real breakage.
//     Same call, same reason, as the unknown-SUBCOMMAND check (see checkDocCommands).
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
//  4. TABLES OF CONTENTS — every entry inside a `<!-- toc -->` block resolves to
//     a heading in that same file. A TOC is precisely the hand-maintained list
//     this header warns about, so it is allowed only in the delimited form that
//     makes it regenerable, and only because this check exists. The LINK check
//     cannot cover it: a bare `#anchor` names no file, so it skips them.
//
// DELIBERATELY NOT CHECKED: prose claims about behaviour ("multi-tenancy is off"),
// which is what the audit's worst findings actually were. No linter catches those.
// The guard buys back the mechanical half so review attention can go to the rest.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/platform"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Scanned counts what each check actually EXAMINED, not what it was handed.
//
// WHY A COUNTER. Every defect this guard has had was the same shape: it scanned
// LESS than it claimed and still printed a clean result — a walk that skipped the
// root, a dot-rule that dropped .github/, a parser that stopped at the first flag,
// at a version string, at `<env>`. None of them changed the file count, so none
// were visible. Reporting what was examined turns the next one into a number that
// moves, and the repo-level test asserts FLOORS on it — so a clean run over a
// shrunken scan fails instead of reassuring.
type Scanned struct {
	Invocations int // `llz …` commands parsed
	Flags       int // flags actually VALIDATED against a resolved command
	Dispatches  int // `gh workflow run` calls parsed
	Links       int // relative links resolved
	SelfLinks   int // absolute links into this repo's own tree, resolved
	TOCEntries  int // generated table-of-contents entries resolved to a heading
	// WorkflowFiles counts the Actions YAML fed through the SAME command/flag
	// validator as the docs. Separate from the Markdown total because a floor on
	// it is what stops the workflow corpus silently emptying — which is how the
	// gap it closes went unnoticed in the first place.
	WorkflowFiles int
}

func (c Scanned) String() string {
	return fmt.Sprintf("%d llz invocation(s) / %d flag(s) across docs + %d workflow file(s), %d workflow dispatch(es), %d link(s), %d toc entr(ies)",
		c.Invocations, c.Flags, c.WorkflowFiles, c.Dispatches, c.Links+c.SelfLinks, c.TOCEntries)
}

type Finding struct {
	File, Detail string
	Line         int
	Kind         string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: [%s] %s", f.File, f.Line, f.Kind, f.Detail)
}

// The scaffold subtree, whose Markdown renders to the instance ROOT.
const (
	scaffoldDirName = "instance-template"
	scaffoldPrefix  = scaffoldDirName + "/"
)

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
var docsGuardSkipDir = map[string]bool{
	".git":           true,
	".terraform":     true, // provider/module cache: third-party READMEs
	".instance-test": true, // the rendered instance `make instance-test` leaves
	".e2e-instance":  true, // the hoisted instance e2e-instantiate builds
	"node_modules":   true,
	"vendor":         true,
	"rendered":       true, // `make render-charts` output
	"bin":            true,
	// A nested git WORKTREE of this same repo. `.claude/worktrees/<branch>` is a
	// supported layout (git worktree add inside the repo), and it is a full second
	// checkout — so walking it double-counts every doc and reports that branch's
	// findings against this one. Observed: 19 findings, all from the worktree,
	// while the real tree was clean; `make instance-test` also fails on it, because
	// copier's clone sees the gitlink and tries a submodule update. Skipping the
	// container rather than deleting anything: it is the operator's live work.
	"worktrees": true,
}

func markdownFiles(repo capability.Repo) ([]string, error) {
	var out []string
	err := repo.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
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
			if p == "." {
				return nil
			}
			if docsGuardSkipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".md") {
			// Already repo-relative — the reader expresses everything under its
			// own root, so the filepath.Rel that derived this is gone.
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// workflowScanRoots are the trees whose workflow YAML actually RUNS `llz`.
// instance-template's is the one that matters most: those files are DELIVERED, so
// a wrong flag there fails in every adopter's CI rather than in ours.
var workflowScanRoots = []string{".github", "instance-template/.github"}

// workflowYAMLFiles collects the Actions YAML whose `run:` steps invoke llz.
//
// WHY THIS CORPUS EXISTS AT ALL. Everything above this line validates MARKDOWN.
// A doc naming a flag that no longer exists misleads a reader; a WORKFLOW naming
// one stops a deployment. That gap is not theoretical: `llz-terraform.yml` once
// invoked `llz ci preflight --deployment` at two sites after an extraction dropped
// that flag, and the branch carrying it went green on every CI check. It surfaced
// as an e2e apply-cluster dying on `unknown flag: --deployment` before Terraform
// ran, which is the most expensive place in this repo to learn it.
//
// The check itself is checkDocCommands, unchanged and shared. That is deliberate:
// a second copy of "resolve the command, then validate its flags" would be a
// second set of tokenizer bugs, and this guard's own history is a list of them
// (an alternation stopping at a version string, a scan stopping at the first
// flag). One validator, two corpora.
func workflowYAMLFiles(repo capability.Repo) ([]string, error) {
	var out []string
	for _, root := range workflowScanRoots {
		err := repo.WalkDir(filepath.FromSlash(root), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				// An absent root is legitimate — this gate also runs in an instance
				// checkout, which has no instance-template/ of its own. Anything
				// else fails, for markdownFiles' reason: silent under-coverage is
				// the defect.
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("walk %s: %w", p, err)
			}
			if d.IsDir() {
				if docsGuardSkipDir[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if n := d.Name(); strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml") {
				out = append(out, filepath.ToSlash(p))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// workflowRunBody reduces an Actions workflow to just its `run:` scalars, each
// line kept at its ORIGINAL line number so a finding points at the real file.
//
// ONLY `run:` — NOT the whole file, and that is the difference between a gate and
// a nuisance. A first cut scanned the raw YAML and reported three findings, all
// from PROSE inside `#` comments: "`llz build --yes` (or …)" and a trailing `)`
// on an inline-code span tokenise as flags, because a YAML comment has none of
// the delimiters the Markdown scanner relies on to know where a command stops.
// None of those would ever execute. `run:` is exactly the set GitHub hands to a
// shell, so it is the set worth being strict about — and comments naming a flag
// stay free to describe history, which the docs doctrine explicitly wants.
//
// A file that does not parse is a FINDING, never a skip: a workflow the guard
// cannot read is a workflow it cannot vouch for, and silent under-coverage is the
// defect this whole guard exists to avoid.
func workflowRunBody(data []byte) (string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return "", err
	}
	// +2 so a run block whose last line is the file's last line cannot index out.
	lines := make([]string, bytes.Count(data, []byte("\n"))+2)
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				v := n.Content[i+1]
				if n.Content[i].Value == "run" && v.Kind == yaml.ScalarNode {
					// WHERE THE CONTENT STARTS DEPENDS ON THE SCALAR STYLE, and
					// getting it wrong is an off-by-one on nearly every step. For a
					// PLAIN scalar (`run: llz …`) v.Line is the content line itself;
					// for a BLOCK scalar (`run: |`) it is the line of the `|` MARKER
					// and content begins on the next line.
					//
					// The first cut assumed the plain case for both. It was verified
					// against llz-terraform.yml:486 — a plain scalar — so the check
					// passed the real-tree proof while reporting every `run: |` step
					// one line high, which is most of them: the deprecated-flag
					// finding pointed at 601 for a flag on 602. A unit test caught
					// what the integration proof could not, because the integration
					// proof happened to use the one style that worked.
					base := v.Line
					if v.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
						base++
					}
					for j, l := range strings.Split(v.Value, "\n") {
						if idx := base + j; idx >= 1 && idx < len(lines) {
							lines[idx] = shellVisible(l)
						}
					}
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&root)
	// Index 1 becomes the first joined line, so element N is file line N.
	return strings.Join(lines[1:], "\n"), nil
}

// shellVisible blanks the parts of a shell line that are not a command being run:
// everything inside single or double quotes, and the `(`/`)`/backtick that wrap a
// substitution. Length is preserved so column-sensitive callers stay aligned.
//
// THE WORKFLOW CORPUS IS SHELL, AND SHELL IS NOT MARKDOWN. Sharing the docs
// tokenizer wholesale produced four findings that were all noise of two kinds,
// and both are about quoting:
//
//	echo "… 'llz build <env> --yes' (or 'llz up <env> --yes') fires."
//	BUCKETS=$(llz ci tf-output bucket_names --json)
//
// The first is PROSE describing a command — it never executes, and `--yes` is a
// perfectly real global flag anyway; the tokenizer only reported it because it ran
// past the closing `'` and swallowed "(or llz" as a flag name. The second is a
// REAL invocation whose last flag picked up the closing paren of the command
// substitution and became `--json)`, a flag no command will ever have.
//
// Blanking quotes fixes both classes at once and costs nothing: flag VALUES are
// quoted (`--region "$REGION"`) and this guard validates flag NAMES, so the
// information removed is information it never used.
func shellVisible(line string) string {
	out := []byte(line)
	var quote byte
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			out[i] = ' '
		case c == '\'' || c == '"':
			quote = c
			out[i] = ' '
		case c == '(' || c == ')' || c == '`':
			out[i] = ' '
		}
	}
	return string(out)
}

// loadWorkflowRuns reads each workflow and reduces it to its run: scalars.
func loadWorkflowRuns(repo capability.Repo, files []string) ([]docFile, []Finding) {
	out := make([]docFile, 0, len(files))
	var bad []Finding
	for _, rel := range files {
		data, err := repo.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			bad = append(bad, Finding{File: rel, Kind: "unreadable",
				Detail: fmt.Sprintf("could not be read (%v) — the guard cannot vouch for this workflow, so the run fails rather than reporting a coverage it does not have", err)})
			continue
		}
		body, err := workflowRunBody(data)
		if err != nil {
			bad = append(bad, Finding{File: rel, Kind: "unparseable",
				Detail: fmt.Sprintf("is not valid YAML (%v) — its `run:` steps cannot be checked", err)})
			continue
		}
		out = append(out, docFile{rel: rel, body: body})
	}
	return out, bad
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
func loadDocs(repo capability.Repo, files []string) ([]docFile, []Finding) {
	docs := make([]docFile, 0, len(files))
	var bad []Finding
	for _, rel := range files {
		data, err := repo.ReadFile(rel)
		if err != nil {
			bad = append(bad, Finding{
				File: rel, Line: 0, Kind: "unreadable",
				Detail: fmt.Sprintf("could not be read (%v) — the guard cannot vouch for this file, so the run fails rather than reporting a coverage it does not have", err),
			})
			continue
		}
		docs = append(docs, docFile{rel: rel, body: string(data)})
	}
	return docs, bad
}

// ── 1. llz FLAGS (on invocations whose command resolves) ─────────────────────

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
		case '|', '#', ';', '&', '>':
			// Shell operators ONLY at a token boundary. Mid-token they are
			// ordinary characters — notably the `>` that CLOSES a placeholder.
			if cur.Len() > 0 {
				cur.WriteRune(c)
				continue
			}
			flush()
			return out // a real operator: `> file`, `| jq`, `# comment`
		case ')':
			// UNLIKE THE OTHERS, `)` ends the invocation even mid-token: unquoted,
			// it closes a `$(…)` substitution, and the command inside stopped one
			// character earlier. It was grouped with the operators above and so was
			// absorbed into whatever token it touched — which made the documented
			// one-liner `eval "$(llz tofu --export)"` report a flag literally named
			// `--export)` plus the rest of the line, because the `"` that follows
			// then opened a quote that ran to the end. Every `$(llz …)` in the
			// corpus had the same hole; nothing caught it because the finding looks
			// like a doc typo rather than a blind spot in the scanner.
			//
			// No placeholder closes with `)` (that is `>`, handled above), and a
			// `)` inside quotes never reaches here — the quote branch consumes it.
			flush()
			return out
		case '\\':
			flush() // a stray continuation marker; tokens continue on the folded line
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}

// blankMermaid replaces the CONTENTS of ```mermaid blocks with empty lines,
// keeping the line count so every finding still points at the right line.
//
// A mermaid node label is a picture caption, not a shell line. The quickstart's
// lifecycle diagram carries a node reading `llz doctor --env`, and the tokeniser
// read the label's closing `"]` as part of the flag — reporting a flag named
// `--env]` that no command could ever have. Diagrams describe commands
// constantly, so this would recur with every new one.
//
// Only ```mermaid is blanked, not every fence: ```bash blocks are exactly the
// copy/paste instructions this guard exists to check.
func blankMermaid(body string) string {
	lines := strings.Split(body, "\n")
	in := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if in {
			if strings.HasPrefix(t, "```") {
				in = false
				continue
			}
			lines[i] = ""
			continue
		}
		if strings.HasPrefix(t, "```mermaid") {
			in = true
		}
	}
	return strings.Join(lines, "\n")
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
func checkDocCommands(docs []docFile, rootCmd *cobra.Command, n *Scanned) []Finding {
	var out []Finding
	for _, d := range docs {
		rel := d.rel
		if isDecisionRecord(rel) {
			continue
		}
		for _, ll := range foldContinuations(blankMermaid(d.body)) {
			for _, m := range llzStartRe.FindAllStringSubmatch(ll.text, -1) {
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
				// Counted only once a REAL command resolved. Counting at the regex
				// match instead folded every prose mention ("the llz binary", "the
				// llz image") into the total, inflating the reported coverage and
				// padding the floor with matches that validate nothing.
				n.Invocations++
				for _, fl := range flags {
					if fl == "--help" || fl == "-h" || !strings.HasPrefix(fl, "--") {
						continue
					}
					// Counted HERE, not per invocation: a truncating parser still
					// finds the invocation, it just stops collecting. Invocation
					// count therefore does NOT move when the scan is blinded —
					// measured, not assumed — so the flag count is the metric.
					n.Flags++
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
						out = append(out, Finding{
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
						out = append(out, Finding{
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

func checkDocWorkflowInputs(repo capability.Repo, docs []docFile, n *Scanned) ([]Finding, error) {
	wfs, err := loadWorkflowInputs(repo)
	if err != nil {
		return nil, err
	}
	if len(wfs) == 0 {
		return nil, nil
	}
	var out []Finding
	for _, d := range docs {
		rel, text := d.rel, blankMermaid(d.body)
		for _, m := range ghRunRe.FindAllStringSubmatchIndex(text, -1) {
			whole := text[m[0]:m[1]]
			sub := ghRunRe.FindStringSubmatch(whole)
			name, rest := sub[1], sub[2]
			line := strings.Count(text[:m[0]], "\n") + 1
			n.Dispatches++
			wf, ok := wfs[name]
			if !ok {
				out = append(out, Finding{File: rel, Line: line, Kind: "workflow",
					Detail: fmt.Sprintf("no workflow named %s in this repo", name)})
				continue
			}
			if !wf.dispatch {
				out = append(out, Finding{File: rel, Line: line, Kind: "workflow",
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
				out = append(out, Finding{File: rel, Line: line, Kind: "workflow-input",
					Detail: fmt.Sprintf("%s does not declare input(s) %s — gh rejects undeclared inputs",
						name, strings.Join(unknown, ", "))})
			}
			if len(missing) > 0 {
				out = append(out, Finding{File: rel, Line: line, Kind: "workflow-input",
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
func loadWorkflowInputs(repo capability.Repo) (map[string]*wfInputs, error) {
	out := map[string]*wfInputs{}
	dirs := []string{
		filepath.Join(".github", "workflows"),
		filepath.Join("instance-template", ".github", "workflows"),
	}
	for _, dir := range dirs {
		entries, err := repo.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
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
			data, err := repo.ReadFile(filepath.Join(dir, e.Name()))
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
	// GitHub accepts three spellings of the trigger list, and only the map form
	// can carry inputs. Treating the other two as "not dispatchable" is a FALSE
	// POSITIVE, not merely under-coverage: docs-guard would report a perfectly
	// valid `gh workflow run` as impossible and fail CI on a correct doc.
	//
	//   on: workflow_dispatch              scalar
	//   on: [push, workflow_dispatch]      sequence
	//   on:                                map — the only form with inputs
	//     workflow_dispatch:
	//       inputs: …
	switch v := on.(type) {
	case string:
		wi.dispatch = v == "workflow_dispatch"
		return wi, nil
	case []any:
		for _, e := range v {
			if fmt.Sprint(e) == "workflow_dispatch" {
				wi.dispatch = true
			}
		}
		return wi, nil
	}
	onMap, ok := on.(map[string]any)
	if !ok {
		return wi, nil // some other shape — no dispatch inputs we can read
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

func checkDocLinks(repo capability.Repo, docs []docFile, n *Scanned) []Finding {
	var out []Finding
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
				n.Links++
				// A leading `/` is ROOT-relative in Markdown — GitHub resolves it
				// against the repo (or, for a rendered scaffold file, the instance)
				// root, not against the file's directory. Joining it to linkDir
				// gave `docs/docs/x.md` and reported a valid link as dead.
				//
				// (It never escaped to the OS root, as one review suggested:
				// filepath.Join CLEANS, so joining the docs dir with an absolute
				// link target yields that target UNDER the docs dir rather than at
				// the filesystem root — contained. The defect is a false POSITIVE,
				// not a filesystem read outside the tree.)
				base := linkDir
				if strings.HasPrefix(path, "/") {
					base, path = "", strings.TrimPrefix(path, "/")
				}
				resolved := filepath.Clean(filepath.Join(base, path))
				// A rendered instance has NOTHING above its root, so a link that
				// climbs past it is dead there however it resolves here. That is
				// how `../../platform-apl/` from apl-values/README.md passed while
				// being dead in every instance.
				//
				// THE REASON FOR THIS CHECK CHANGED, AND IT STILL EARNS ITS PLACE.
				// It used to be a SAFETY check as well: pathExists did
				// os.Stat(filepath.Join(root, p)), so a climbing link wandered out
				// of the repo and could match a same-named directory by pure
				// coincidence. The read-repo fence closed that — pathExists now
				// refuses anything outside the tree rather than resolving it.
				//
				// What the fence does NOT do is produce this FINDING. It would
				// answer "does not exist", and the guard would report a broken
				// link rather than the specific, actionable one: the link climbs
				// above the instance root and needs an absolute template URL.
				// Deleting this because "the fence handles it" would trade a
				// precise diagnostic for a vague one.
				if rendered && strings.HasPrefix(resolved, "..") {
					out = append(out, Finding{File: rel, Line: i + 1, Kind: "link",
						Detail: fmt.Sprintf("%s climbs above the instance root — dead in a rendered instance; use an absolute template URL", target)})
					continue
				}
				if pathExists(repo, resolved) {
					continue
				}
				if rendered && pathExists(repo, filepath.Join(scaffoldDirName, resolved)) {
					continue // template-owned; renders into the instance beside this file
				}
				if rendered && platform.RenderTimeArtifact[filepath.ToSlash(resolved)] {
					continue // written during render, so absent here by construction
				}
				out = append(out, Finding{File: rel, Line: i + 1, Kind: "link",
					Detail: fmt.Sprintf("%s does not exist", target)})
			}
		}
	}
	out = append(out, checkSelfRepoLinks(repo, docs, n)...)
	out = append(out, checkDeliveredDocLinks(repo, docs, n)...)
	out = append(out, checkDocTOCs(docs, n)...)
	return out
}

// ── 3b. generated tables of contents ─────────────────────────────────────────

// A TOC is a hand-maintained list of the exact kind this file's header warns
// about: it is true when written and rots the moment a heading is renamed, in a
// long doc where nobody re-reads the top. The link check above cannot catch it —
// it skips `#`-anchors entirely, because a bare anchor names no file.
//
// So the rule is: a doc may only carry a TOC if it is DELIMITED, and every entry
// inside the delimiters must resolve to a heading in that same file. That makes
// the list regenerable (the delimiters say where) and checkable (this function),
// which is what separates it from the hand-maintained lists we refuse elsewhere.
//
// Only the delimited block is checked. Prose that happens to link a section is a
// normal cross-reference, not a claim to be exhaustive.
var (
	tocBlockRe  = regexp.MustCompile(`(?s)<!--\s*toc\s*-->(.*?)<!--\s*/toc\s*-->`)
	tocEntryRe  = regexp.MustCompile(`\[[^\]]*\]\(#([^)]+)\)`)
	mdHeadingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	anchorStrip = regexp.MustCompile("`([^`]*)`")
	anchorBold  = regexp.MustCompile(`\*+([^*]*)\*+`)
	anchorLink  = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// Underscore SURVIVES. GitHub keeps it, and this repo's headings are full of
	// `workflow_call`, `promotion_rank`, `ha_role` — dropping it silently turned
	// every such anchor into a false positive on the first run of this check.
	// Variation selectors and ZWJ SURVIVE. github-slugger removes a fixed list
	// of punctuation/symbols; it never touches these, so `⚠️ …` slugs to a
	// leading (invisible) U+FE0F. Stripping them is a mismatch the oracle test
	// catches on three real headings in this repo.
	anchorPunct = regexp.MustCompile(`[^\p{L}\p{N}_\s\x{200D}\x{FE00}-\x{FE0F}-]`)
)

// githubAnchor reproduces github-slugger, which is what GitHub actually runs.
//
// The subtle half is the LAST step. github-slugger does `.replace(/ /g, '-')` —
// each space becomes its own hyphen — NOT a collapse of whitespace runs. It
// matters whenever punctuation sits between two spaces, because removing the
// punctuation leaves two spaces behind and therefore TWO hyphens:
//
//	"Writing / rotating secrets — dual-write"
//	  -> writing--rotating-secrets--dual-write      (github, and now us)
//	  -> writing-rotating-secrets-dual-write        (a \s+ collapse: WRONG)
//
// This repo's headings are full of that shape (" — ", " / "), so the collapse
// rule broke a whole class of anchors. It went unnoticed at first because the
// TOC generator and this checker shared the rule: the guard agreed with the
// thing it was checking, which is the exact failure this file's header warns
// about. TestGithubAnchor_MatchesGithubSlugger pins the output against slugs
// captured from github-slugger over every heading in the repo.
func githubAnchor(text string) string {
	t := anchorStrip.ReplaceAllString(text, "$1")
	t = anchorBold.ReplaceAllString(t, "$1")
	t = anchorLink.ReplaceAllString(t, "$1")
	t = strings.ToLower(strings.TrimSpace(t))
	t = anchorPunct.ReplaceAllString(t, "")
	// Tabs and newlines cannot appear in a heading line, so ' ' is the only
	// whitespace left to map — one hyphen each.
	return strings.ReplaceAll(t, " ", "-")
}

// docAnchors returns every anchor the headings of a doc define. A repeated
// heading text gets GitHub's `-1`, `-2` … suffixes, so those are registered too.
// docHeading is one heading with the anchor GitHub will give it.
type docHeading struct {
	Level  int
	Text   string
	Anchor string
	Line   int
}

// docHeadings walks a document's headings IN ORDER, assigning each the anchor
// GitHub would. Both the TOC generator (`llz ci gen-toc`) and the TOC checker
// call it, so there is exactly ONE slug implementation in the repo.
//
// That sharing is deliberate but not free: a checker that shares its rule with
// the generator cannot catch a bug IN the rule — which is precisely how the
// whitespace-collapse bug survived when the generator was a separate Python
// script. TestGithubAnchor_MatchesGithubSlugger is what covers that gap, by
// pinning githubAnchor against slugs captured from the real implementation.
//
// De-duplication is over ALL headings in document order, before any level
// filtering — GitHub numbers the second "Notes" as `notes-1` whether or not a
// TOC lists it.
func docHeadings(body string) []docHeading {
	var out []docHeading
	seen, fence := map[string]int{}, false
	for i, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fence = !fence
			continue
		}
		if fence {
			continue
		}
		m := mdHeadingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		base := githubAnchor(m[2])
		if base == "" {
			continue
		}
		a := base
		if n := seen[base]; n > 0 {
			a = fmt.Sprintf("%s-%d", base, n)
		}
		seen[base]++
		out = append(out, docHeading{
			Level: len(m[1]), Text: strings.TrimRight(m[2], " "), Anchor: a, Line: i + 1,
		})
	}
	return out
}

func docAnchors(body string) map[string]bool {
	out := map[string]bool{}
	for _, h := range docHeadings(body) {
		out[h.Anchor] = true
	}
	return out
}

func checkDocTOCs(docs []docFile, n *Scanned) []Finding {
	var out []Finding
	for _, d := range docs {
		block := tocBlockRe.FindStringSubmatchIndex(d.body)
		if block == nil {
			continue
		}
		anchors := docAnchors(d.body)
		inner := d.body[block[2]:block[3]]
		base := strings.Count(d.body[:block[2]], "\n") + 1
		for i, line := range strings.Split(inner, "\n") {
			for _, m := range tocEntryRe.FindAllStringSubmatch(line, -1) {
				n.TOCEntries++
				if anchors[m[1]] {
					continue
				}
				out = append(out, Finding{
					File: d.rel, Line: base + i, Kind: "toc",
					Detail: fmt.Sprintf("#%s matches no heading — regenerate the block between the toc markers", m[1]),
				})
			}
		}
	}
	return out
}

// selfRepoBlobRe matches a link into THIS repo's own tree at `main` — the form
// docs use to point at source that is not delivered locally. `<org>` is loose on
// purpose so a fork's URLs are checked too; the ref is pinned to `main` because a
// version-tagged permalink names a HISTORICAL tree this checkout cannot vouch for.
var selfRepoBlobRe = regexp.MustCompile(
	`^https://github\.com/[^/]+/lke-landing-zone/(?:blob|tree)/main/([^)\s#]+)`)

// checkSelfRepoLinks verifies that an absolute link into this repo's own tree
// resolves to a real path.
//
// This gap was found the expensive way: a blanket sed that stripped
// `instance-template/` from delivered docs also stripped it from an ABSOLUTE URL,
// where the prefix was correct — producing a 404 in the bootstrap runbook that the
// guard could not see, because it only ever looked at relative links. An absolute
// URL into our own tree is just as checkable as a relative one, and is exactly what
// docs use for source that is NOT delivered locally.
func checkSelfRepoLinks(repo capability.Repo, docs []docFile, n *Scanned) []Finding {
	var out []Finding
	for _, d := range docs {
		for i, line := range strings.Split(d.body, "\n") {
			for _, m := range docsGuardLinkRe.FindAllStringSubmatch(line, -1) {
				sm := selfRepoBlobRe.FindStringSubmatch(m[1])
				if sm == nil {
					continue
				}
				n.SelfLinks++
				if pathExists(repo, filepath.FromSlash(sm[1])) {
					continue
				}
				detail := fmt.Sprintf("%s does not exist in this repo", sm[1])
				if alt := filepath.Join(scaffoldDirName, filepath.FromSlash(sm[1])); pathExists(repo, alt) {
					detail += fmt.Sprintf(" — did you mean %s? (the scaffold lives under %s/ in the TEMPLATE repo, even though it renders to the instance root)", filepath.ToSlash(alt), scaffoldDirName)
				}
				out = append(out, Finding{File: d.rel, Line: i + 1, Kind: "self-link", Detail: detail})
			}
		}
	}
	return out
}

// checkDeliveredDocLinks is the check that matters most: it evaluates the kept
// docs against the keep-set they will actually ship with. A runbook that links a
// sibling runbook is fine; one that links a doc `deliver-docs` prunes is fine too
// (the rewrite repoints it) — but only if the rewrite can SEE it, which is what
// the audit found it could not do from the instance root.
func checkDeliveredDocLinks(repo capability.Repo, docs []docFile, n *Scanned) []Finding {
	var out []Finding
	delivered := func(rel string) bool {
		if !strings.HasPrefix(rel, "docs/") {
			return false
		}
		top := strings.SplitN(strings.TrimPrefix(rel, "docs/"), "/", 2)[0]
		return platform.DeliveredDocs[top]
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
				// Root-relative here too — see the note in checkDocLinks. Fixing
				// only one of the two resolvers left this one reporting a valid
				// `/docs/x.md` as dead, which the test caught.
				dbase := dir
				if strings.HasPrefix(path, "/") {
					dbase, path = "", strings.TrimPrefix(path, "/")
				}
				resolved := filepath.Clean(filepath.Join(dbase, path))
				// A link OUT of docs/ cannot be repointed by deliver-docs (its
				// rewrite is docs/-relative) and will not resolve in an instance.
				if !strings.HasPrefix(resolved, "docs/") {
					out = append(out, Finding{File: rel, Line: i + 1, Kind: "delivered-link",
						Detail: fmt.Sprintf("%s escapes docs/ — dead in a delivered instance; use an absolute template URL", target)})
					continue
				}
				// Inside docs/: either still delivered (relative link works) or
				// pruned (deliver-docs repoints it). Only a target that exists in
				// NEITHER is broken.
				if !pathExists(repo, resolved) {
					out = append(out, Finding{File: rel, Line: i + 1, Kind: "delivered-link",
						Detail: fmt.Sprintf("%s does not exist", target)})
				}
			}
		}
	}
	return out
}

// Options selects which of the three checks run. All-false runs everything, which
// is what CI and `make docs-guard` want; the skips exist for bisecting a noisy
// change, not as a supported way to weaken the gate.
type Options struct {
	SkipCommands  bool
	SkipWorkflows bool
	SkipLinks     bool
}

// Report is one run's outcome: what was checked, over how much, and what is wrong.
//
// READ AND FOUND ARE SEPARATE FIELDS, and that is load-bearing rather than tidy.
// Printing "checked N" beside an error citing a larger M invited the reader to
// assume the gap was rounding, when it is exactly the thing that matters — files
// the guard could not open and therefore cannot vouch for.
type Report struct {
	Findings []Finding
	Scanned  Scanned
	Read     int // docs successfully loaded
	Total    int // Markdown files found
}

// Unreadable is how many files were found but could not be read.
func (r Report) Unreadable() int { return r.Total - r.Read }

// Scope describes what the run can vouch for, naming unreadable files when there
// are any.
func (r Report) Scope() string {
	if r.Unreadable() == 0 {
		return fmt.Sprintf("%d file(s)", r.Read)
	}
	return fmt.Sprintf("%d of %d file(s) — %d unreadable", r.Read, r.Total, r.Unreadable())
}

// Run validates every Markdown file under root against the repo it documents.
//
// rootCmd IS WHY THIS EXTENSION CAN NEVER BE EXTERNAL. The command/flag check
// resolves each documented `llz …` invocation against the LIVE cobra tree and
// asks that tree whether the flag exists and whether it takes a value — including
// flags marked hidden, which a `--help` sweep cannot see (that is how a
// deprecated-but-working `--env` was once reported as removed). An argv-shaped
// tool would have to re-derive the command tree from help text, which is the
// second-implementation-of-a-shared-rule bug this package already has scars from.
// The catalog's "36 of 57 candidates need in-process Go" is this, concretely.
func Run(root string, opts Options, rootCmd *cobra.Command) (Report, error) {
	repo := capability.RepoForGate(Extension(), root)
	files, err := markdownFiles(repo)
	if err != nil {
		return Report{}, err
	}
	// Read once. An unreadable file becomes a FINDING here rather than a silent
	// skip in each of the three checks below — a guard that cannot read a doc must
	// not report that it checked it.
	docs, findings := loadDocs(repo, files)
	rep := Report{Findings: findings, Read: len(docs), Total: len(files)}
	if !opts.SkipCommands {
		rep.Findings = append(rep.Findings, checkDocCommands(docs, rootCmd, &rep.Scanned)...)

		// The same validator over the workflow corpus. These are the invocations
		// that RUN, and nothing checked them until an `unknown flag` killed an e2e
		// apply-cluster on a branch whose eleven CI checks were all green.
		wfFiles, werr := workflowYAMLFiles(repo)
		if werr != nil {
			return rep, werr
		}
		wfDocs, wfBad := loadWorkflowRuns(repo, wfFiles)
		rep.Scanned.WorkflowFiles = len(wfDocs)
		rep.Findings = append(rep.Findings, wfBad...)
		rep.Findings = append(rep.Findings, checkDocCommands(wfDocs, rootCmd, &rep.Scanned)...)
	}
	if !opts.SkipWorkflows {
		f, err := checkDocWorkflowInputs(repo, docs, &rep.Scanned)
		if err != nil {
			return rep, err
		}
		rep.Findings = append(rep.Findings, f...)
	}
	if !opts.SkipLinks {
		rep.Findings = append(rep.Findings, checkDocLinks(repo, docs, &rep.Scanned)...)
	}
	sort.Slice(rep.Findings, func(i, j int) bool {
		if rep.Findings[i].File != rep.Findings[j].File {
			return rep.Findings[i].File < rep.Findings[j].File
		}
		return rep.Findings[i].Line < rep.Findings[j].Line
	})
	return rep, nil
}
