package main

// ci_docs_facts.go — `llz ci docs-facts`: render load-bearing facts from the Go
// constants that define them into marked blocks in the docs, and fail CI when a
// block drifts from its source.
//
// WHY THIS EXISTS, measured rather than assumed. The 104-file documentation audit
// (PR #406) found 30 defects. `llz ci docs-guard` now catches 11 of them — the
// mechanical half: dead links, unknown flags, undeclared workflow inputs. It
// cannot catch the other 19, and that is where ALL FOUR of the audit's criticals
// were:
//
//	the OpenBao break-glass recipe naming :8200 and VAULT_ADDR
//	the Loki playbook stating multi-tenancy was OFF
//	the onboarding step that pointed core.hooksPath at a directory that
//	  does not exist, silently disabling the pre-commit secret guard
//	the volume sweep documented without --env, which leaks every
//	  relabelled volume
//
// Each was a doc RESTATING a fact that already exists as a constant in this
// binary, then drifting from it as the code moved. No linter catches "this
// sentence is wrong". But a doc that RENDERS the fact cannot drift from it, and
// that turns the most expensive finding class in the audit into a red build.
//
// SCOPE, deliberately narrow. This is not a documentation generator. It owns
// small, exactly-quotable facts — a port, a path, an env pairing, a tenant name,
// a flag's own help — where the doc's job is to repeat the code verbatim. Prose
// ABOUT those facts stays hand-written, because prose is where the judgement is
// and no generator improves it.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// docFact is one renderable fact. `render` reads from the live code — a constant,
// a helper, or the cobra tree — so the block cannot drift without this failing.
type docFact struct {
	name   string
	what   string // one line, shown in `--list`
	source string // where the truth lives, for a reader who wants to check
	render func(root *cobra.Command) string
}

// docFacts is the registry. Adding one is: name it, say where the truth lives,
// return the text. Keep them SMALL — a fact worth generating is one a doc would
// otherwise transcribe by hand and get wrong.
var docFacts = []docFact{
	{
		name:   "openbao.in-pod-env",
		what:   "the env an in-pod `bao` invocation needs (the F1 critical)",
		source: "baoLoopbackEnv() in openbao.go",
		render: func(*cobra.Command) string {
			// Rendered from the SAME helper every in-pod caller uses, so a doc
			// cannot show one address while the code uses another — which is
			// exactly what shipped: the runbook said VAULT_ADDR=…:8200, an
			// mTLS listener that rejects a caller with no client certificate.
			var b strings.Builder
			for _, kv := range baoLoopbackEnv() {
				b.WriteString(kv + "\n")
			}
			return strings.TrimRight(b.String(), "\n")
		},
	},
	{
		name:   "openbao.listeners",
		what:   "which OpenBao listener an in-pod caller may use, and why",
		source: "openbaoLoopbackPort + the chart's listener block",
		render: func(*cobra.Command) string {
			return "[::]:8200        pod network — mTLS, a CLIENT CERTIFICATE IS REQUIRED\n" +
				"127.0.0.1:" + openbaoLoopbackPort + "   loopback    — TLS, no client certificate"
		},
	},
	{
		name:   "loki.tenants",
		what:   "the Loki tenant each writer uses (the F2 critical)",
		source: "defaultCollectorTenant + defaultAuditTenant",
		render: func(*cobra.Command) string {
			return "apl-core's collector (every landing-zone namespace)   X-Scope-OrgID: " + defaultCollectorTenant + "\n" +
				"the OpenBao promtail sidecar (audit log)              X-Scope-OrgID: " + defaultAuditTenant
		},
	},
	{
		name:   "hooks.install-path",
		what:   "where `llz hooks` installs, and the local escape hatch (the F3 critical)",
		source: "runHooksInstall + runPrecommit in hooks.go",
		render: func(*cobra.Command) string {
			// `llz hooks` writes into git's OWN hooks dir. The audit found a doc
			// telling operators to point core.hooksPath elsewhere, which makes
			// git ignore this file and run no hooks at all — silently.
			return "installed hook:    <git hooks dir>/" + gitPreCommitHookName + "  (git rev-parse --git-path hooks)\n" +
				"your extra checks: " + localPreCommitHook + "  (executable; runs after the built-in gate)"
		},
	},
	{
		name:   "reap-volumes.env",
		what:   "what --env buys on the volume sweep (the F4 critical)",
		source: "the flag's own Usage string on `llz ci reap-volumes`",
		render: func(root *cobra.Command) string {
			// Straight from the flag, so the doc cannot describe it differently
			// from the CLI. The audit found every documented invocation omitting
			// it, which silently leaks every relabelled volume.
			return "--env  " + flagUsage(root, []string{"ci", "reap-volumes"}, "env")
		},
	},
}

// flagUsage returns a flag's Usage text from the live command tree, so a doc
// quoting a flag quotes what the CLI actually prints.
func flagUsage(root *cobra.Command, path []string, flag string) string {
	cmd, _, err := root.Find(path)
	if err != nil || cmd == nil {
		return "(unresolved command " + strings.Join(path, " ") + ")"
	}
	f := cmd.Flags().Lookup(flag)
	if f == nil {
		return "(no --" + flag + " on " + cmd.CommandPath() + ")"
	}
	return f.Usage
}

// ── the markers ──────────────────────────────────────────────────────────────
//
// A block is delimited by HTML comments, so it renders as nothing on GitHub and
// the doc reads normally. The fenced code block between them is the payload.

const (
	factOpenFmt = "<!-- llz:fact %s -->"
	factClose   = "<!-- /llz:fact -->"
)

var factBlockRe = regexp.MustCompile(`(?s)<!-- llz:fact ([a-z0-9.-]+) -->\n(.*?)<!-- /llz:fact -->`)

// renderFactBlock is the full replacement text for one marked block: the marker,
// a fenced payload, the closing marker. Pure, so the shape is unit-tested.
func renderFactBlock(name, body string) string {
	return fmt.Sprintf(factOpenFmt, name) + "\n```text\n" + strings.TrimRight(body, "\n") + "\n```\n" + factClose
}

func ciDocsFactsCmd() *cobra.Command {
	var root string
	var check, list, strict bool
	c := &cobra.Command{
		Use:   "docs-facts",
		Short: "render load-bearing facts from their Go source into marked doc blocks",
		Long: "Docs restate facts that already exist as constants in this binary — the\n" +
			"OpenBao in-pod env, the Loki tenants, where `llz hooks` installs, what\n" +
			"--env buys on the volume sweep. Every one of those drifted, and all four\n" +
			"were CRITICAL findings in the documentation audit.\n\n" +
			"A block marked `<!-- llz:fact <name> -->` is rendered from the live code, so\n" +
			"it cannot drift. --check verifies without writing (CI); the default rewrites\n" +
			"in place. --list shows the registry.\n\n" +
			"It owns exactly-quotable facts only. Prose ABOUT them stays hand-written —\n" +
			"that is where the judgement is, and no generator improves it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if list {
				return listDocFacts(cmd.OutOrStdout())
			}
			return runDocsFacts(root, check, strict, cmd.Root())
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	c.Flags().BoolVar(&check, "check", false, "verify blocks match their source and exit non-zero on drift; write nothing")
	c.Flags().BoolVar(&list, "list", false, "list the fact registry and what each renders")
	c.Flags().BoolVar(&strict, "strict", false, "also require every registered fact to be rendered somewhere (whole-repo scans only)")
	return c
}

func listDocFacts(w interface{ Write([]byte) (int, error) }) error {
	for _, f := range docFacts {
		fmt.Fprintf(w, "%-24s %s\n%-24s source: %s\n\n", f.name, f.what, "", f.source)
	}
	return nil
}

func factByName(name string) (docFact, bool) {
	for _, f := range docFacts {
		if f.name == name {
			return f, true
		}
	}
	return docFact{}, false
}

func runDocsFacts(root string, check, strict bool, rootCmd *cobra.Command) error {
	files, err := markdownFiles(root)
	if err != nil {
		return err
	}
	docs, unreadable := loadDocs(root, files)
	// An unreadable doc is a finding, not a skip — same rule as docs-guard, and
	// for the same reason: a tool that cannot read a file must not report it clean.
	var drifted, rewrote []string
	for _, u := range unreadable {
		drifted = append(drifted, u.File+": "+u.Detail)
	}

	seen := map[string]int{}
	for _, d := range docs {
		out := factBlockRe.ReplaceAllStringFunc(d.body, func(m string) string {
			sm := factBlockRe.FindStringSubmatch(m)
			name := sm[1]
			seen[name]++
			f, ok := factByName(name)
			if !ok {
				drifted = append(drifted, fmt.Sprintf("%s: unknown fact %q — see `llz ci docs-facts --list`", d.rel, name))
				return m
			}
			want := renderFactBlock(name, f.render(rootCmd))
			if want != m {
				drifted = append(drifted, fmt.Sprintf("%s: fact %q is stale — its source is %s", d.rel, name, f.source))
			}
			return want
		})
		if out == d.body {
			continue
		}
		if !check {
			if err := os.WriteFile(filepath.Join(root, d.rel), []byte(out), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", d.rel, err)
			}
			rewrote = append(rewrote, d.rel)
		}
	}

	// A fact nobody renders is dead weight that will rot unnoticed — exactly the
	// shape this command exists to prevent. But it is a REPO-level invariant: over
	// an arbitrary subtree (or a test fixture holding one doc) every other fact is
	// trivially "unused", so enforcing it there is noise, not signal. --strict is
	// what the Makefile and CI pass when scanning the whole repo.
	var orphans []string
	if strict {
		for _, f := range docFacts {
			if seen[f.name] == 0 {
				orphans = append(orphans, f.name)
			}
		}
	}
	sort.Strings(orphans)
	for _, o := range orphans {
		drifted = append(drifted, fmt.Sprintf("fact %q is in the registry but no doc renders it — delete it or use it", o))
	}

	if check {
		if len(drifted) > 0 {
			for _, d := range drifted {
				fmt.Println(d)
			}
			return fmt.Errorf("docs-facts: %d block(s) drifted from their source — run `make docs-facts` to regenerate", len(drifted))
		}
		fmt.Printf("docs-facts: %d fact(s) rendered in %d doc(s) — all match their source.\n",
			len(docFacts), countFactDocs(seen))
		return nil
	}
	if len(rewrote) > 0 {
		sort.Strings(rewrote)
		fmt.Printf("docs-facts: regenerated %d block(s) in %s\n", len(rewrote), strings.Join(rewrote, ", "))
	} else {
		fmt.Printf("docs-facts: %d fact(s) already match their source.\n", len(docFacts))
	}
	// Orphans are still an error in write mode: regenerating cannot fix them.
	if len(orphans) > 0 {
		for _, o := range orphans {
			fmt.Printf("  unused fact: %s\n", o)
		}
		return fmt.Errorf("docs-facts: %d registered fact(s) are rendered by no doc", len(orphans))
	}
	return nil
}

func countFactDocs(seen map[string]int) int {
	n := 0
	for _, c := range seen {
		n += c
	}
	return n
}
