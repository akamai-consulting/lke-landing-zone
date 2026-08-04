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
			name:         "a sequence WITHOUT workflow_dispatch is not dispatchable",
			yaml:         "on: [push, pull_request]\n",
			wantDispatch: false,
		},
		{
			// GitHub accepts three spellings and only the map form carries
			// inputs. Reading the other two as non-dispatchable is a FALSE
			// POSITIVE — docs-guard would call a valid `gh workflow run`
			// impossible and fail CI on a correct doc.
			name:         "scalar shorthand is dispatchable",
			yaml:         "on: workflow_dispatch\n",
			wantDispatch: true,
		},
		{
			name:         "sequence shorthand is dispatchable",
			yaml:         "on: [push, workflow_dispatch]\n",
			wantDispatch: true,
		},
		{
			name:         "block-sequence shorthand is dispatchable",
			yaml:         "on:\n  - push\n  - workflow_dispatch\n",
			wantDispatch: true,
		},
		{
			name:         "a scalar that is some OTHER trigger is not dispatchable",
			yaml:         "on: push\n",
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
			docs, bad := loadDocs(root, []string{tc.rel})
			if len(bad) != 0 {
				t.Fatalf("fixture unreadable: %v", bad)
			}
			got := checkDocCommands(docs, newRootCmd(), &docsScanned{})
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
			docs, _ := loadDocs(root, []string{"docs/run.md"})
			got, err := checkDocWorkflowInputs(root, docs, &docsScanned{})
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
		if got := checkDeliveredDocLinksFrom(t, root, "docs/runbooks/r.md"); len(got) != 0 {
			t.Fatalf("expected no findings, got %v", got)
		}
	})

	t.Run("a link escaping docs/ is dead after delivery", func(t *testing.T) {
		writeMD(t, root, "docs/runbooks/r.md", "see [the chart](../../kubernetes-charts/README.md)\n")
		got := checkDeliveredDocLinksFrom(t, root, "docs/runbooks/r.md")
		if len(got) == 0 {
			t.Fatal("expected a finding for a link escaping docs/")
		}
		if !strings.Contains(got[0].Detail, "escapes docs/") {
			t.Errorf("finding = %q, want it to mention escaping docs/", got[0].Detail)
		}
	})

	t.Run("a link to a nonexistent doc is reported", func(t *testing.T) {
		writeMD(t, root, "docs/playbooks/p.md", "see [gone](gone.md)\n")
		got := checkDeliveredDocLinksFrom(t, root, "docs/playbooks/p.md")
		if len(got) == 0 || !strings.Contains(got[0].Detail, "does not exist") {
			t.Fatalf("expected a does-not-exist finding, got %v", got)
		}
	})

	t.Run("a doc that is NOT delivered is not held to this rule", func(t *testing.T) {
		writeMD(t, root, "docs/secrets.md", "see [charts](../kubernetes-charts/README.md)\n")
		if got := checkDeliveredDocLinksFrom(t, root, "docs/secrets.md"); len(got) != 0 {
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
	docs, unreadable := loadDocs(root, files)
	var n docsScanned
	findings = append(findings, unreadable...)
	findings = append(findings, checkDocCommands(docs, newRootCmd(), &n)...)
	wfFindings, err := checkDocWorkflowInputs(root, docs, &n)
	if err != nil {
		t.Fatalf("workflow inputs: %v", err)
	}
	findings = append(findings, wfFindings...)
	findings = append(findings, checkDocLinks(root, docs, &n)...)

	// COVERAGE FLOORS — a TRIPWIRE for gross blinding, not a precise gate.
	// Be clear about what they do and do not buy, because the temptation is to
	// treat a green run as proof of coverage, which is the exact mistake this
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
	if n.flags < 200 {
		t.Errorf("only %d flag(s) validated (was 210) — the parser has been blinded; a clean run over a shrunken scan is the failure mode this guard keeps having", n.flags)
	}
	if n.invocations < 600 {
		t.Errorf("only %d llz invocation(s) resolved (was 761) — commands are no longer being recognised", n.invocations)
	}
	if n.dispatches < 10 {
		t.Errorf("only %d workflow dispatch(es) scanned (was 15)", n.dispatches)
	}
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

// checkDeliveredDocLinksFrom reads then checks, so the delivered-link tests read
// like the production path (load once, then check) instead of re-reading.
func checkDeliveredDocLinksFrom(t *testing.T, root string, rels ...string) []docFinding {
	t.Helper()
	docs, bad := loadDocs(root, rels)
	if len(bad) != 0 {
		t.Fatalf("fixture unreadable: %v", bad)
	}
	return checkDeliveredDocLinks(root, docs, &docsScanned{})
}

// A guard that cannot READ a doc must not report that it checked it. Every
// checker used to `continue` past an unreadable file, so a permission bit
// produced a clean "N file(s) OK" over a smaller N — the same false-green this
// whole guard exists to prevent.
func TestLoadDocs_UnreadableFileIsAFindingNotASilentSkip(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "docs/fine.md", "# fine")
	writeMD(t, root, "docs/locked.md", "# secret")
	locked := filepath.Join(root, "docs", "locked.md")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("cannot chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	if os.Geteuid() == 0 {
		t.Skip("running as root — mode 0000 is still readable")
	}

	docs, bad := loadDocs(root, []string{"docs/fine.md", "docs/locked.md"})
	if len(docs) != 1 || docs[0].rel != filepath.Join("docs", "fine.md") {
		t.Errorf("readable docs = %+v, want just docs/fine.md", docs)
	}
	if len(bad) != 1 || bad[0].Kind != "unreadable" {
		t.Fatalf("an unreadable file must produce exactly one 'unreadable' finding, got %+v", bad)
	}
	if !strings.Contains(bad[0].File, "locked.md") {
		t.Errorf("the finding must name the file, got %q", bad[0].File)
	}
}

// markdownFiles must fail on a walk error rather than returning a short list
// that later prints as a clean run.
func TestMarkdownFiles_WalkErrorFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — an unreadable dir is still traversable")
	}
	root := t.TempDir()
	writeMD(t, root, "docs/fine.md", "# fine")
	blocked := filepath.Join(root, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "inner", "hidden.md"), []byte("# hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Skipf("cannot chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	if _, err := markdownFiles(root); err == nil {
		t.Error("markdownFiles swallowed a walk error — it must fail closed, not under-cover silently")
	}
}

// The command scan is line-based, so a multi-line `llz … \` invocation had only
// its FIRST line checked — and multi-line is exactly the shape of the copy/paste
// blocks most likely to drift (quickstart.md's flagship `llz env add` spans three
// lines; two were unguarded).
func TestFoldContinuations(t *testing.T) {
	got := foldContinuations("llz env add lab \\\n  --region us-sea \\\n  --nope\nunrelated\n")
	if len(got) == 0 || got[0].num != 1 {
		t.Fatalf("first logical line should start at 1, got %+v", got)
	}
	for _, want := range []string{"--region us-sea", "--nope"} {
		if !strings.Contains(got[0].text, want) {
			t.Errorf("continuation %q not folded into the first line: %q", want, got[0].text)
		}
	}
	// A finding must still point at the line a reader will look at.
	if got[0].num != 1 {
		t.Errorf("folded line reported at %d, want the STARTING line 1", got[0].num)
	}
}

// The old whole-command regex stopped at the first token it could not classify,
// so a value like `v1.33.6+lke7` or a CIDR ended the match and every flag after it
// went unchecked — on one line or many.
func TestInvocationTokens(t *testing.T) {
	for _, tc := range []struct {
		name, rest string
		want       []string
	}{
		{"a dotted/plus version does not end the scan",
			" env add lab --k8s-version v1.33.6+lke7 --nope",
			[]string{"env", "add", "lab", "--k8s-version", "v1.33.6+lke7", "--nope"}},
		{"a CIDR does not end the scan",
			" env add lab --runner-ipv4-cidrs 203.0.113.0/24 --nope",
			[]string{"env", "add", "lab", "--runner-ipv4-cidrs", "203.0.113.0/24", "--nope"}},
		{"a pipe ends the command",
			" env list --json | jq -r '.[]'",
			[]string{"env", "list", "--json"}},
		{"a closing backtick ends the command",
			" doctor --env lab` and then something",
			[]string{"doctor", "--env", "lab"}},
		{"a quoted value stays one token",
			` openbao set secret/x k="a b" --yes`,
			[]string{"openbao", "set", "secret/x", "k=a b", "--yes"}},
		{"a comment ends the command",
			" build lab --yes  # dispatches terraform.yml",
			[]string{"build", "lab", "--yes"}},
		{
			// THE placeholder case. `<env>` is the most common thing a real
			// invocation contains (92 across the docs) and `<` sits at a token
			// start — so terminating on it, as an earlier cut did to catch input
			// redirects, blinded the scan past the placeholder on nearly every
			// command in the corpus.
			"an angle-bracket placeholder does not end the scan",
			" env add <env> --region us-sea --nope",
			[]string{"env", "add", "<env>", "--region", "us-sea", "--nope"}},
		{"placeholders with a slash and a pipe stay whole",
			" openbao get <active|standby> <secret/path> <key>",
			[]string{"openbao", "get", "<active|standby>", "<secret/path>", "<key>"}},
		{"a redirect at a token boundary still ends the command",
			` completion zsh > "${fpath[1]}/_llz"`,
			[]string{"completion", "zsh"}},
		{"an owner/name pair of placeholders is two tokens",
			" new <owner>/<name> --push",
			[]string{"new", "<owner>/<name>", "--push"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := invocationTokens(tc.rest)
			if len(got) != len(tc.want) {
				t.Fatalf("tokens = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("token %d = %q, want %q (full: %q)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// instance-template/ Markdown renders to the instance ROOT, so its links must be
// judged as they will appear there. An earlier cut SKIPPED that whole tree on the
// grounds that the links "resolve in a rendered instance" — nothing judged them in
// a rendered instance either, so the one tree the guard ignored is precisely where
// links shipped dead to adopters.
func TestCheckDocLinks_InstanceTemplateIsJudgedAsRendered(t *testing.T) {
	root := t.TempDir()
	// Repo layout: docs/ at the root (copied into the instance at render),
	// plus the scaffold subtree.
	writeMD(t, root, "docs/quickstart.md", "# qs")
	writeMD(t, root, "instance-template/kubernetes-custom/README.md", "# custom")

	for _, tc := range []struct{ name, rel, body, wantHit string }{
		{
			// docs/ lives at the repo ROOT, not under instance-template/ — the
			// link is correct and must not be reported.
			name: "a link into docs/ resolves against the repo root",
			rel:  "instance-template/AGENTS.md",
			body: "see [qs](docs/quickstart.md)\n",
		},
		{
			// A sibling scaffold file renders alongside, so it satisfies the link.
			name: "a link to a template-owned sibling resolves",
			rel:  "instance-template/README.md",
			body: "see [custom](kubernetes-custom/README.md)\n",
		},
		{
			name:    "a link to nothing is reported",
			rel:     "instance-template/AGENTS.md",
			body:    "see [gone](docs/nope.md)\n",
			wantHit: "does not exist",
		},
		{
			// The false negative that let `../../platform-apl/` pass: the probe
			// walked OUT of the repo and coincidentally landed on a real dir.
			name:    "a link climbing above the instance root is reported",
			rel:     "instance-template/apl-values/README.md",
			body:    "see [shared](../../platform-apl/)\n",
			wantHit: "climbs above the instance root",
		},
		{
			// deliver-docs writes docs/README.md at render time, so its absence
			// here is by construction, not breakage.
			name: "a render-time artifact is not a dead link",
			rel:  "instance-template/README.md",
			body: "see [docs](docs/README.md)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeMD(t, root, tc.rel, tc.body)
			docs, bad := loadDocs(root, []string{tc.rel})
			if len(bad) != 0 {
				t.Fatalf("fixture unreadable: %v", bad)
			}
			got := checkDocLinks(root, docs, &docsScanned{})
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

// The guard does NOT report an unknown top-level command, and that is a decision
// with a measurement behind it, not an oversight: `.llz/commands.yaml` lets an
// instance define its own verbs, and extending-llz.md documents two of them
// (`llz smoke`, `llz psql`). Reporting unknown commands would flag the very doc
// that teaches the mechanism. This pins the behaviour so nobody "fixes" it into
// a false positive — and pins that a KNOWN command's flags are still checked.
func TestCheckDocCommands_UserDefinedCommandsAreNotReported(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "docs/extending.md",
		"```bash\nllz smoke                 # runs: bash hack/smoke.sh\n"+
			"llz psql --db readonly    # extra args appended to argv\n```\n")
	docs, bad := loadDocs(root, []string{"docs/extending.md"})
	if len(bad) != 0 {
		t.Fatalf("fixture unreadable: %v", bad)
	}
	if got := checkDocCommands(docs, newRootCmd(), &docsScanned{}); len(got) != 0 {
		t.Errorf("user-defined commands must not be reported, got %v", got)
	}

	// ...while a REAL command's bad flag is still caught, so the exemption is
	// scoped to unknown commands and has not silenced the check.
	writeMD(t, root, "docs/real.md", "`llz doctor --not-a-flag`\n")
	docs, _ = loadDocs(root, []string{"docs/real.md"})
	if got := checkDocCommands(docs, newRootCmd(), &docsScanned{}); len(got) != 1 {
		t.Errorf("a known command's unknown flag must still be reported, got %v", got)
	}
}

// An ABSOLUTE link into this repo's own tree is as checkable as a relative one,
// and the guard used to ignore it entirely. That gap shipped a 404: a blanket sed
// stripped `instance-template/` from delivered docs and also stripped it from a
// template-repo URL, where the prefix was correct.
func TestCheckSelfRepoLinks(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "instance-template/.github/workflows/x.yml", "on: push")
	writeMD(t, root, "docs/real.md", "# real")

	base := "https://github.com/akamai-consulting/lke-landing-zone"
	for _, tc := range []struct{ name, body, wantHit string }{
		{
			name:    "a path that is not in the tree is reported",
			body:    "[wf](" + base + "/blob/main/.github/workflows/x.yml)\n",
			wantHit: "does not exist in this repo",
		},
		{
			// The hint is the whole point: this exact confusion caused the 404.
			name:    "and it names the instance-template/ alternative",
			body:    "[wf](" + base + "/blob/main/.github/workflows/x.yml)\n",
			wantHit: "did you mean instance-template/.github/workflows/x.yml",
		},
		{
			name: "a path that IS in the tree passes",
			body: "[wf](" + base + "/blob/main/instance-template/.github/workflows/x.yml)\n",
		},
		{
			name: "a tree URL to a directory passes",
			body: "[d](" + base + "/tree/main/docs)\n",
		},
		{
			// A version-tagged permalink names a HISTORICAL tree; this checkout
			// cannot vouch for it, so it must not be judged.
			name: "a version-pinned permalink is not judged",
			body: "[old](" + base + "/blob/v0.0.32/docs/gone.md)\n",
		},
		{
			name: "a fork's URL is checked too",
			body: "[wf](https://github.com/someorg/lke-landing-zone/blob/main/instance-template/.github/workflows/x.yml)\n",
		},
		{
			name: "an unrelated GitHub URL is ignored",
			body: "[x](https://github.com/other/project/blob/main/nope.md)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeMD(t, root, "docs/probe.md", tc.body)
			docs, bad := loadDocs(root, []string{"docs/probe.md"})
			if len(bad) != 0 {
				t.Fatalf("fixture unreadable: %v", bad)
			}
			got := checkSelfRepoLinks(root, docs, &docsScanned{})
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
