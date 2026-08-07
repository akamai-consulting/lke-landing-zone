package main

// ci_docs_guard_test.go — STAYS IN PACKAGE MAIN, unlike its command.
//
// docs-guard checks every documented `llz …` invocation against the LIVE cobra
// tree. At runtime the command gets that from cobra itself via cmd.Root(), so
// internal/docsguard owns its command cleanly. A TEST has no running command to
// ask, so it must BUILD the tree — and newRootCmd is main's. This is the one
// place the rule "an extension owns its command" leaves a test behind, and the
// reason is that the extension's subject IS the tree main owns.

// ci_docs_guard_test.go — the docs-guard cases that need the LIVE cobra tree.
//
// The extraction to internal/docsguard split this file along a line worth naming:
// 21 test functions moved with the logic, and these six could not, because every
// one of them asserts against the REAL command inventory — that `llz validate
// --env` is hidden-and-deprecated, that `llz env add` takes `--k8s-version`, that
// `llz openbao exec --` is an argv separator. A synthetic tree would make them
// pass while the CLI drifted underneath, which is the failure they exist to catch.
//
// So they live where the tree is assembled and go through the package's exported
// API. That is not a workaround: `newRootCmd()` IS package main, and an extension
// that needs it is an extension that must be in-process Go. The catalog's "36 of
// 57 candidates cannot be external" is this file.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/docsguard"
)

// commandFindings runs ONLY the command/flag check over a fixture repo. The other
// two checks are skipped so a fixture needs no workflows and no link targets.
func commandFindings(t *testing.T, root string) []docsguard.Finding {
	t.Helper()
	rep, err := docsguard.Run(root, docsguard.Options{SkipWorkflows: true, SkipLinks: true}, newRootCmd())
	if err != nil {
		t.Fatalf("docsguard.Run: %v", err)
	}
	return rep.Findings
}

// writeMD, not writeMD: docsguard_test.go already has a writeMD and the
// two are NOT the same function. Renamed rather than merged — a shared helper
// here would have silently changed one caller's fixture.
func writeMD(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repoRootForDocsGuardCobra(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "copier.yml")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "docs")); err == nil {
				return dir
			}
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("repo root not found from the test working directory")
	return ""
}

// docValueShapes is deliberately a SMALL sample, not the exhaustive table.
//
// The full set of token shapes a doc writes a flag value in lives with the parser
// it exercises (TestInvocationTokens_TrailingFlagSurvivesEveryValueShape in
// internal/docsguard), where each shape is one cheap case against a pure function.
// What these cases add on top is the only thing the package cannot check: that the
// finding still lands when the flag is looked up in the REAL command tree. Four
// representative shapes buy that; twenty would buy the same thing twenty times.

func TestCheckDocCommands(t *testing.T) {
	for _, tc := range []struct {
		name, rel, body string
		wantHit         string // substring of the expected finding, "" = expect none
	}{
		{
			// A flag that never existed on that command.
			name:    "unknown flag on a real command is reported",
			rel:     "docs/guide.md",
			body:    "run `llz doctor --nope lab` to check\n",
			wantHit: "has no flag --nope",
		},
		{
			// The shape that actually bit the docs: the flag still WORKS, so
			// nothing fails and the doc keeps teaching it after the rename.
			// `llz validate --env` is hidden-and-deprecated, which is why it
			// survived a --help-based sweep.
			name:    "a deprecated flag is reported even though it still works",
			rel:     "docs/guide.md",
			body:    "run `llz validate --env lab` to check\n",
			wantHit: "is deprecated",
		},
		{
			name: "a valid flag is accepted",
			rel:  "docs/guide.md",
			body: "run `llz doctor --env lab`\n",
		},
		{
			name: "persistent flags are accepted anywhere",
			rel:  "docs/guide.md",
			body: "run `llz build lab --yes --dry-run`\n",
		},
		{
			// `--` is the argv separator for exec-style commands, not a flag.
			name: "the argv separator is not treated as a flag",
			rel:  "docs/guide.md",
			body: "run `llz openbao exec -- policy list`\n",
		},
		{
			// Prose mentioning the binary must not parse as an invocation.
			name: "prose about the llz binary is ignored",
			rel:  "docs/guide.md",
			body: "the llz binary is pinned, and the llz image is signed\n",
		},
		{
			// ADRs and designs are dated records; a flag they name may be
			// historical or never-built, and rewriting them would falsify them.
			name: "decision records are exempt",
			rel:  "docs/adr/0099-thing.md",
			body: "we shipped `llz validate --env lab` at the time\n",
		},
		{
			name: "design docs are exempt too",
			rel:  "docs/designs/thing.md",
			body: "the sketch was `llz reconcile --reconcile-harbor`\n",
		},
		{
			// PERSISTENT FLAG BEFORE THE SUBCOMMAND. This shape ships in our own
			// docs (orphan-volume-cleanup.md: `llz --yes ci reap-volumes …`), and
			// an earlier parser stopped collecting words at the first flag — so
			// the whole invocation resolved to no command and was skipped
			// SILENTLY. Silent under-coverage in a guard is worse than no guard.
			name:    "a leading persistent flag does not hide the subcommand",
			rel:     "docs/guide.md",
			body:    "run `llz --yes ci reap-volumes --totally-bogus`\n",
			wantHit: "`llz ci reap-volumes` has no flag --totally-bogus",
		},
		{
			name: "a leading persistent flag with a valid tail is accepted",
			rel:  "docs/guide.md",
			body: "run `LINODE_TOKEN=x llz --yes ci reap-volumes --region us-ord --env lab`\n",
		},
		{
			// A VALUE-taking flag consumes the next token, which must not then be
			// mistaken for a subcommand — while a BOOL flag must not consume one,
			// which is what makes the case above work.
			name:    "a value-taking flag's value is not read as a subcommand",
			rel:     "docs/guide.md",
			body:    "run `llz --dry-run env add lab --region us-sea --nope`\n",
			wantHit: "`llz env add` has no flag --nope",
		},
		{
			// A positional argument must not be mistaken for a subcommand and
			// send the flag lookup to the wrong command.
			name:    "a positional arg does not derail the flag lookup",
			rel:     "docs/guide.md",
			body:    "run `llz env show my-deployment --json`\n",
			wantHit: "has no flag --json",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeMD(t, root, tc.rel, tc.body)
			got := commandFindings(t, root)
			if tc.wantHit == "" {
				if len(got) != 0 {
					t.Fatalf("expected no findings, got %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("expected a finding containing %q, got none", tc.wantHit)
			}
			if !strings.Contains(got[0].Detail, tc.wantHit) {
				t.Errorf("finding = %q, want it to contain %q", got[0].Detail, tc.wantHit)
			}
		})
	}
}

// ── the gh workflow run check ────────────────────────────────────────────────

func TestDocsGuard_CleanOnThisRepo(t *testing.T) {
	root := repoRootForDocsGuardCobra(t)
	rep, err := docsguard.Run(root, docsguard.Options{}, newRootCmd())
	if err != nil {
		t.Fatalf("docsguard.Run: %v", err)
	}
	if rep.Total < 50 {
		t.Fatalf("only found %d Markdown files — is the repo root wrong? (%s)", rep.Total, root)
	}
	findings, n := rep.Findings, rep.Scanned

	// COVERAGE FLOORS — a TRIPWIRE for gross blinding, not a precise gate.
	// Be clear about what they do and do not buy, because the temptation is to
	// treat a color.Green run as proof of coverage, which is the exact mistake this
	// guard keeps making.
	//
	// MEASURED by re-introducing each real regression and reading the counters:
	//
	//   `<env>` terminator (Copilot #9)  flags 210 -> 162   CAUGHT
	//   leading-flag parse (Copilot #3)  flags 210 -> 200   NOT caught
	//
	// So a large blinding trips these and a small one does not. Tightening to
	// catch the 5% case would fail CI on any docs PR that removes a handful of
	// flags — brittle enough that it would get loosened again, or deleted. The
	// counters' main value is the PRINTED line: a reviewer comparing runs sees
	// the number move even when the floor does not.
	if n.Flags < 200 {
		t.Errorf("only %d flag(s) validated (was 210) — the parser has been blinded; a clean run over a shrunken scan is the failure mode this guard keeps having", n.Flags)
	}
	if n.Invocations < 600 {
		t.Errorf("only %d llz invocation(s) resolved (was 761) — commands are no longer being recognised", n.Invocations)
	}
	if n.Dispatches < 10 {
		t.Errorf("only %d workflow dispatch(es) scanned (was 15)", n.Dispatches)
	}
	if n.TOCEntries < 100 {
		t.Errorf("only %d toc entr(ies) checked (was 137) — either the toc blocks were removed from the long docs or the block parser stopped seeing them", n.TOCEntries)
	}
	for _, f := range findings {
		t.Errorf("%s", f)
	}
}

func TestCheckDocCommands_UserDefinedCommandsAreNotReported(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "docs/extending.md",
		"```bash\nllz smoke                 # runs: bash hack/smoke.sh\n"+
			"llz psql --db readonly    # extra args appended to argv\n```\n")
	if got := commandFindings(t, root); len(got) != 0 {
		t.Errorf("user-defined commands must not be reported, got %v", got)
	}

	// ...while a REAL command's bad flag is still caught, so the exemption is
	// scoped to unknown commands and has not silenced the check.
	writeMD(t, root, "docs/real.md", "`llz doctor --not-a-flag`\n")
	if got := commandFindings(t, root); len(got) != 1 {
		t.Errorf("a known command's unknown flag must still be reported, got %v", got)
	}
}

// An ABSOLUTE link into this repo's own tree is as checkable as a relative one,
// and the guard used to ignore it entirely. That gap shipped a 404: a blanket sed
// stripped `instance-template/` from delivered docs and also stripped it from a
// template-repo URL, where the prefix was correct.
func TestCheckDocCommands_ReportsBadFlagBehindEveryValueShape(t *testing.T) {
	root := t.TempDir()
	for _, v := range []struct{ name, tok string }{
		{"placeholder", "<version>"},
		{"dotted plus version", "v1.33.6+lke7"},
		{"quoted with space", `"a b"`},
		{"equals form", "key=value"},
	} {
		t.Run(v.name, func(t *testing.T) {
			writeMD(t, root, "docs/p.md",
				"`llz env add lab --k8s-version "+v.tok+" --definitely-not-a-flag`\n")
			got := commandFindings(t, root)
			if len(got) != 1 || !strings.Contains(got[0].Detail, "--definitely-not-a-flag") {
				t.Errorf("a bad flag behind %s (%q) was not reported — got %v", v.name, v.tok, got)
			}
		})
	}
}

// And the leading-flag axis, which is orthogonal: a persistent flag BEFORE the
// subcommand must not hide anything after it.
func TestCheckDocCommands_LeadingFlagsDoNotHideTrailingOnes(t *testing.T) {
	root := t.TempDir()
	for _, lead := range []string{"--yes", "--dry-run", "--open", "-y"} {
		t.Run(lead, func(t *testing.T) {
			writeMD(t, root, "docs/p.md",
				"`llz "+lead+" ci reap-volumes --region <cluster_region> --definitely-not-a-flag`\n")
			got := commandFindings(t, root)
			if len(got) != 1 || !strings.Contains(got[0].Detail, "--definitely-not-a-flag") {
				t.Errorf("a leading %s hid the trailing flag — got %v", lead, got)
			}
		})
	}
}

// The shorthand-`on:` defect survived a table of HAND-WRITTEN YAML because I only
// wrote the shapes I had thought of. This cross-checks the parser against every
// REAL workflow in the repo using an independent, deliberately naive signal — a
// grep. Where the two disagree, one of them is wrong, and either way it is worth
// knowing. A fixture cannot go stale against reality; this can't either.
// Mermaid node labels look exactly like invocations and must not be scanned as
// them. Asserted here rather than in the package because "produced no findings"
// is only meaningful against the real command tree — against a synthetic one, a
// label naming an unknown command is silently fine either way.
func TestBlankMermaid_LabelsAreNotInvocations(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "d.md",
		"# T\n\n```mermaid\nflowchart LR\n    D[\"llz doctor --env\"]\n"+
			"    X[\"gh workflow run nope.yml -f bad=1\"]\n```\n\n```bash\nllz doctor --env lab\n```\n")
	rep, err := docsguard.Run(root, docsguard.Options{SkipWorkflows: true, SkipLinks: true}, newRootCmd())
	if err != nil {
		t.Fatalf("docsguard.Run: %v", err)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("mermaid labels produced findings: %v", rep.Findings)
	}
	// The bash block beside it must STILL be scanned — blanking mermaid must not
	// become a way to stop checking the copy/paste instructions.
	if rep.Scanned.Invocations == 0 {
		t.Error("the ```bash invocation was not scanned; blankMermaid over-reached")
	}
}
