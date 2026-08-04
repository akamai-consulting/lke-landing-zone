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
			var findings []docFinding
			files, err := markdownFiles(root)
			if err != nil {
				return err
			}
			if !skipCommands {
				findings = append(findings, checkDocCommands(root, files, cmd.Root())...)
			}
			if !skipWorkflows {
				f, err := checkDocWorkflowInputs(root, files)
				if err != nil {
					return err
				}
				findings = append(findings, f...)
			}
			if !skipLinks {
				findings = append(findings, checkDocLinks(root, files)...)
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
				return fmt.Errorf("docs-guard: %d finding(s) across %d Markdown file(s)", len(findings), len(files))
			}
			fmt.Printf("docs-guard: %d Markdown file(s) OK — commands, workflow inputs and links all resolve.\n", len(files))
			return nil
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	c.Flags().BoolVar(&skipCommands, "skip-commands", false, "skip the llz command/flag check")
	c.Flags().BoolVar(&skipWorkflows, "skip-workflows", false, "skip the gh workflow run input check")
	c.Flags().BoolVar(&skipLinks, "skip-links", false, "skip the relative-link check")
	return c
}

func markdownFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// NEVER skip the root itself — it is routinely passed as "." or "..",
			// whose basename starts with a dot and would match the rule below,
			// skipping the entire walk and reporting a clean "0 files OK".
			if p == root {
				return nil
			}
			// Skip build artifacts and vendored trees. Any DOT-directory is
			// out: .git, .terraform (whose provider tarballs carry READMEs
			// full of links relative to THEIR repo), and .instance-test —
			// the rendered instance `make instance-test` leaves behind, which
			// would otherwise be scanned as if it were source. No documentation
			// this guard should judge lives in a dot-directory.
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			switch d.Name() {
			case "node_modules", "vendor", "rendered", "bin":
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

// ── 1. llz commands + flags ──────────────────────────────────────────────────

// Matches `llz` followed by subcommand words and any flags, inside prose or a
// fenced block. Stops at a pipe, backtick, or newline so a shell pipeline does
// not bleed into the token list.
var llzInvocationRe = regexp.MustCompile(`(?:^|[^\w./-])llz((?:\s+(?:--?[\w-]+(?:=[^\s` + "`" + `|]*)?|[a-z][a-z0-9-]*))+)`)

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

// Prose regularly reads "the llz binary", "an llz workload", "the llz image".
// Those parse as a subcommand and are not one; the check only reports a token
// run whose FIRST word is a real top-level command, so ordinary English is
// invisible to it rather than needing an ignore list.
func checkDocCommands(root string, files []string, rootCmd *cobra.Command) []docFinding {
	var out []docFinding
	for _, rel := range files {
		if isDecisionRecord(rel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range llzInvocationRe.FindAllStringSubmatch(line, -1) {
				fields := strings.Fields(m[1])
				var words, flags []string
				for _, f := range fields {
					if f == "--" {
						// The argv separator (`llz openbao exec -- kv get …`),
						// not a flag. Everything after it is the inner command.
						break
					}
					if strings.HasPrefix(f, "-") {
						flags = append(flags, strings.SplitN(f, "=", 2)[0])
						continue
					}
					if len(flags) > 0 {
						continue // a flag VALUE, not a subcommand
					}
					words = append(words, f)
				}
				if len(words) == 0 {
					continue
				}
				cur, _, err := rootCmd.Find(words[:1])
				if err != nil || cur == rootCmd {
					continue // not a command at all — prose
				}
				// Walk as deep as the words go; a word that is not a subcommand
				// is treated as a positional argument and ends the descent.
				depth := 1
				for depth < len(words) {
					next, _, err := cur.Find(words[depth : depth+1])
					if err != nil || next == cur {
						break
					}
					cur = next
					depth++
				}
				for _, fl := range flags {
					if fl == "--help" || fl == "-h" || !strings.HasPrefix(fl, "--") {
						continue
					}
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
							File: rel, Line: i + 1, Kind: "flag",
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
							File: rel, Line: i + 1, Kind: "deprecated-flag",
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

func checkDocWorkflowInputs(root string, files []string) ([]docFinding, error) {
	wfs, err := loadWorkflowInputs(root)
	if err != nil {
		return nil, err
	}
	if len(wfs) == 0 {
		return nil, nil
	}
	var out []docFinding
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		text := string(data)
		for _, m := range ghRunRe.FindAllStringSubmatchIndex(text, -1) {
			whole := text[m[0]:m[1]]
			sub := ghRunRe.FindStringSubmatch(whole)
			name, rest := sub[1], sub[2]
			line := strings.Count(text[:m[0]], "\n") + 1
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
			continue // a repo without one of these is fine
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

func checkDocLinks(root string, files []string) []docFinding {
	var out []docFinding
	for _, rel := range files {
		dir := filepath.Dir(rel)
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
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
				resolved := filepath.Clean(filepath.Join(dir, path))
				if pathExists(filepath.Join(root, resolved)) {
					continue
				}
				// instance-template/ links resolve against a RENDERED instance,
				// where docs/ has been delivered — judge them there instead.
				if strings.HasPrefix(rel, "instance-template"+string(filepath.Separator)) {
					continue
				}
				out = append(out, docFinding{File: rel, Line: i + 1, Kind: "link",
					Detail: fmt.Sprintf("%s does not exist", target)})
			}
		}
	}
	out = append(out, checkDeliveredDocLinks(root, files)...)
	return out
}

// checkDeliveredDocLinks is the check that matters most: it evaluates the kept
// docs against the keep-set they will actually ship with. A runbook that links a
// sibling runbook is fine; one that links a doc `deliver-docs` prunes is fine too
// (the rewrite repoints it) — but only if the rewrite can SEE it, which is what
// the audit found it could not do from the instance root.
func checkDeliveredDocLinks(root string, files []string) []docFinding {
	var out []docFinding
	delivered := func(rel string) bool {
		if !strings.HasPrefix(rel, "docs/") {
			return false
		}
		top := strings.SplitN(strings.TrimPrefix(rel, "docs/"), "/", 2)[0]
		return docsKeep[top]
	}
	for _, rel := range files {
		if !delivered(rel) {
			continue
		}
		dir := filepath.Dir(rel)
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
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
