package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── workflow_dispatch input parsing ──────────────────────────────────────────

func TestParseWorkflowDispatchInputs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		yaml         string
		wantDispatch bool
		wantNames    []string
		wantRequired []string
	}{
		{
			name: "required input with no default is required",
			yaml: `
name: T
on:
  workflow_dispatch:
    inputs:
      region:
        description: 'deployment'
        required: true
        type: string
      verbose:
        required: false
        type: boolean
`,
			wantDispatch: true,
			wantNames:    []string{"region", "verbose"},
			wantRequired: []string{"region"},
		},
		{
			// A required input carrying a default is satisfiable without being
			// passed, so a doc that omits it is not wrong.
			name: "required WITH a default is not something a doc must pass",
			yaml: `
on:
  workflow_dispatch:
    inputs:
      region:
        required: true
        default: primary
        type: string
`,
			wantDispatch: true,
			wantNames:    []string{"region"},
			wantRequired: nil,
		},
		{
			// Hyphenated names are legal and were the source of a false positive
			// when the first cut of this parser matched [a-z_] only.
			name: "hyphenated input names are captured",
			yaml: `
on:
  workflow_dispatch:
    inputs:
      fail-on-unhealthy:
        required: false
        type: boolean
      assert-loki:
        required: false
        type: boolean
`,
			wantDispatch: true,
			wantNames:    []string{"assert-loki", "fail-on-unhealthy"},
		},
		{
			name:         "workflow_dispatch with no inputs still dispatches",
			yaml:         "on:\n  workflow_dispatch:\n",
			wantDispatch: true,
		},
		{
			name:         "no workflow_dispatch trigger",
			yaml:         "on:\n  push:\n    branches: [main]\n",
			wantDispatch: false,
		},
		{
			// YAML 1.1 folds a bare `on` to the boolean true in some parsers;
			// a workflow may also quote it. Every spelling must resolve.
			name:         "quoted on key",
			yaml:         "\"on\":\n  workflow_dispatch:\n    inputs:\n      region:\n        required: true\n",
			wantDispatch: true,
			wantNames:    []string{"region"},
			wantRequired: []string{"region"},
		},
		{
			name:         "on as a sequence has no dispatch inputs",
			yaml:         "on: [push, pull_request]\n",
			wantDispatch: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wi, err := parseWorkflowDispatchInputs([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if wi.dispatch != tc.wantDispatch {
				t.Errorf("dispatch = %v, want %v", wi.dispatch, tc.wantDispatch)
			}
			for _, n := range tc.wantNames {
				if !wi.names[n] {
					t.Errorf("input %q not captured (got %v)", n, wi.names)
				}
			}
			if len(wi.names) != len(tc.wantNames) {
				t.Errorf("input count = %d (%v), want %d", len(wi.names), wi.names, len(tc.wantNames))
			}
			for _, r := range tc.wantRequired {
				if !wi.required[r] {
					t.Errorf("input %q should be required (got %v)", r, wi.required)
				}
			}
			if len(wi.required) != len(tc.wantRequired) {
				t.Errorf("required count = %d (%v), want %d", len(wi.required), wi.required, len(tc.wantRequired))
			}
		})
	}
}

// ── the command/flag check ───────────────────────────────────────────────────

func writeMD(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

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
			got := checkDocCommands(root, []string{tc.rel}, newRootCmd())
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

func TestCheckDocWorkflowInputs(t *testing.T) {
	root := t.TempDir()
	wf := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wf, "bootstrap-openbao.yml"), []byte(
		"on:\n  workflow_dispatch:\n    inputs:\n      region:\n        required: true\n        type: string\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, body, wantHit string
	}{
		{
			// The F10 shape, verbatim.
			name:    "undeclared inputs are reported",
			body:    "```bash\ngh workflow run bootstrap-openbao.yml -f environment=lab -f mode=init\n```\n",
			wantHit: "does not declare input(s) environment, mode",
		},
		{
			name:    "a missing required input is reported",
			body:    "```bash\ngh workflow run bootstrap-openbao.yml\n```\n",
			wantHit: "requires input(s) region",
		},
		{
			name: "a correct dispatch is accepted",
			body: "```bash\ngh workflow run bootstrap-openbao.yml -f region=lab\n```\n",
		},
		{
			name: "the --field spelling is accepted",
			body: "```bash\ngh workflow run bootstrap-openbao.yml --field region=lab\n```\n",
		},
		{
			name:    "an unknown workflow is reported",
			body:    "```bash\ngh workflow run nope.yml -f region=lab\n```\n",
			wantHit: "no workflow named nope.yml",
		},
		{
			name: "a line-continued dispatch is read whole",
			body: "```bash\ngh workflow run bootstrap-openbao.yml \\\n  -f region=lab\n```\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeMD(t, root, "docs/run.md", tc.body)
			got, err := checkDocWorkflowInputs(root, []string{"docs/run.md"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantHit == "" {
				if len(got) != 0 {
					t.Fatalf("expected no findings, got %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("expected a finding containing %q, got none", tc.wantHit)
			}
			joined := ""
			for _, f := range got {
				joined += f.Detail + "\n"
			}
			if !strings.Contains(joined, tc.wantHit) {
				t.Errorf("findings =\n%s\nwant one containing %q", joined, tc.wantHit)
			}
		})
	}
}

// ── the delivered-docs link check ────────────────────────────────────────────

// The check that matters most: a KEPT doc linking OUT of docs/ is dead in every
// rendered instance, because deliver-docs' rewrite is docs/-relative and cannot
// repoint it.
func TestCheckDeliveredDocLinks(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "docs/quickstart.md", "# qs")
	writeMD(t, root, "docs/secrets.md", "# secrets")

	t.Run("a link to a pruned doc is fine — deliver-docs repoints it", func(t *testing.T) {
		writeMD(t, root, "docs/runbooks/r.md", "see [secrets](../secrets.md)\n")
		if got := checkDeliveredDocLinks(root, []string{"docs/runbooks/r.md"}); len(got) != 0 {
			t.Fatalf("expected no findings, got %v", got)
		}
	})

	t.Run("a link escaping docs/ is dead after delivery", func(t *testing.T) {
		writeMD(t, root, "docs/runbooks/r.md", "see [the chart](../../kubernetes-charts/README.md)\n")
		got := checkDeliveredDocLinks(root, []string{"docs/runbooks/r.md"})
		if len(got) == 0 {
			t.Fatal("expected a finding for a link escaping docs/")
		}
		if !strings.Contains(got[0].Detail, "escapes docs/") {
			t.Errorf("finding = %q, want it to mention escaping docs/", got[0].Detail)
		}
	})

	t.Run("a link to a nonexistent doc is reported", func(t *testing.T) {
		writeMD(t, root, "docs/playbooks/p.md", "see [gone](gone.md)\n")
		got := checkDeliveredDocLinks(root, []string{"docs/playbooks/p.md"})
		if len(got) == 0 || !strings.Contains(got[0].Detail, "does not exist") {
			t.Fatalf("expected a does-not-exist finding, got %v", got)
		}
	})

	t.Run("a doc that is NOT delivered is not held to this rule", func(t *testing.T) {
		writeMD(t, root, "docs/secrets.md", "see [charts](../kubernetes-charts/README.md)\n")
		if got := checkDeliveredDocLinks(root, []string{"docs/secrets.md"}); len(got) != 0 {
			t.Fatalf("secrets.md is referenced, not delivered — got %v", got)
		}
	})
}

// The guard is only worth having if it is green on the tree it ships with.
// This is the regression test for the whole audit: if a future change reintroduces
// a bad flag, a bad workflow input, or a dead link, this fails in the package's
// own test run rather than waiting for CI.
func TestDocsGuard_CleanOnThisRepo(t *testing.T) {
	root := repoRootForDocsGuard(t)
	files, err := markdownFiles(root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) < 50 {
		t.Fatalf("only found %d Markdown files — is the repo root wrong? (%s)", len(files), root)
	}
	var findings []docFinding
	findings = append(findings, checkDocCommands(root, files, newRootCmd())...)
	wfFindings, err := checkDocWorkflowInputs(root, files)
	if err != nil {
		t.Fatalf("workflow inputs: %v", err)
	}
	findings = append(findings, wfFindings...)
	findings = append(findings, checkDocLinks(root, files)...)
	for _, f := range findings {
		t.Errorf("%s", f)
	}
}

func repoRootForDocsGuard(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if pathExists(filepath.Join(dir, "copier.yml")) && pathExists(filepath.Join(dir, "docs")) {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("repo root not found from the test working directory")
	return ""
}

// `make instance-test` leaves a fully rendered instance at .instance-test/, and
// `tofu init` leaves vendored provider READMEs under .terraform/. Both are full of
// Markdown whose links resolve against a DIFFERENT repo — scanning them made the
// guard fail on a clean tree, purely because another target had run first.
//
// The first fix skipped EVERY dot-directory, which silently dropped .github/'s four
// real docs and took the guard from 105 files to 101 while still reporting success.
// So this pins both directions: artifacts out, .github IN.
func TestMarkdownFiles_SkipsArtifactsButKeepsDotGithub(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "docs/real.md", "# real")
	writeMD(t, root, ".github/PULL_REQUEST_TEMPLATE.md", "# pr template")
	writeMD(t, root, ".github/ISSUE_TEMPLATE/bug_report.md", "# bug")
	writeMD(t, root, ".github/workflows/AGENTS.md", "# wf agents")
	writeMD(t, root, ".instance-test/instance/README.md", "[x](nope.md)")
	writeMD(t, root, "a/.terraform/providers/p/README.md", "[y](nope.md)")
	writeMD(t, root, ".e2e-instance/README.md", "[e](nope.md)")
	writeMD(t, root, "node_modules/pkg/README.md", "[z](nope.md)")
	writeMD(t, root, "rendered/out/README.md", "[w](nope.md)")
	writeMD(t, root, "vendor/v/README.md", "[v](nope.md)")

	files, err := markdownFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[filepath.ToSlash(f)] = true
	}
	for _, want := range []string{
		"docs/real.md",
		".github/PULL_REQUEST_TEMPLATE.md",
		".github/ISSUE_TEMPLATE/bug_report.md",
		".github/workflows/AGENTS.md",
	} {
		if !got[want] {
			t.Errorf("%s is a real doc and must be scanned (got %v)", want, files)
		}
	}
	for f := range got {
		for _, artifact := range []string{".instance-test/", ".terraform/", ".e2e-instance/", "node_modules/", "rendered/", "vendor/"} {
			if strings.Contains(f, artifact) {
				t.Errorf("%s is a build artifact and must not be scanned", f)
			}
		}
	}
}

// The Makefile invokes the guard with `--root .` or `--root ..`, whose BASENAME
// starts with a dot. An earlier cut of the dot-directory skip matched the root
// itself and skipped the entire walk — reporting a clean "0 Markdown file(s) OK"
// while checking nothing at all. A guard that passes vacuously is worse than none.
func TestMarkdownFiles_RootPassedAsDotIsNotSkipped(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "docs/real.md", "# real")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{".", "./"} {
		files, err := markdownFiles(r)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 {
			t.Errorf("--root %q walked %d files, want 1 — the root must never be skipped", r, len(files))
		}
	}
	// ..: the parent of a temp dir may hold other tests' dirs, so assert only
	// that the walk is not short-circuited to nothing.
	if err := os.Chdir(filepath.Join(root, "docs")); err != nil {
		t.Fatal(err)
	}
	files, err := markdownFiles("..")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Error(`--root ".." walked 0 files — the root was skipped`)
	}
}
