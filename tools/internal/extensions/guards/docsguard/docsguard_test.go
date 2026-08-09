package docsguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"

	"github.com/spf13/cobra"
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
			docs, _ := loadDocs(gRepo(root), []string{"docs/run.md"})
			got, err := checkDocWorkflowInputs(gRepo(root), docs, &Scanned{})
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
func repoRootForDocsGuard(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		// Probing THROUGH a reader fenced at the candidate: the search walks
		// upward, so each candidate is its own tree.
		if r := gRepo(dir); pathExists(r, "copier.yml") && pathExists(r, "docs") {
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

	files, err := markdownFiles(gRepo(root))
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
		files, err := markdownFiles(gRepo(r))
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
	files, err := markdownFiles(gRepo(".."))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Error(`--root ".." walked 0 files — the root was skipped`)
	}
}

// checkDeliveredDocLinksFrom reads then checks, so the delivered-link tests read
// like the production path (load once, then check) instead of re-reading.
func checkDeliveredDocLinksFrom(t *testing.T, root string, rels ...string) []Finding {
	t.Helper()
	docs, bad := loadDocs(gRepo(root), rels)
	if len(bad) != 0 {
		t.Fatalf("fixture unreadable: %v", bad)
	}
	return checkDeliveredDocLinks(gRepo(root), docs, &Scanned{})
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

	docs, bad := loadDocs(gRepo(root), []string{"docs/fine.md", "docs/locked.md"})
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

	if _, err := markdownFiles(gRepo(root)); err == nil {
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
			docs, bad := loadDocs(gRepo(root), []string{tc.rel})
			if len(bad) != 0 {
				t.Fatalf("fixture unreadable: %v", bad)
			}
			got := checkDocLinks(gRepo(root), docs, &Scanned{})
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
			docs, bad := loadDocs(gRepo(root), []string{"docs/probe.md"})
			if len(bad) != 0 {
				t.Fatalf("fixture unreadable: %v", bad)
			}
			got := checkSelfRepoLinks(gRepo(root), docs, &Scanned{})
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

// ── preemptive property tests ────────────────────────────────────────────────
//
// Three separate review rounds found the same defect in different clothes: the
// scan stopped early on a token shape nobody had considered — a leading `--yes`,
// a value like `v1.33.6+lke7`, then `<env>`. Each was fixed with a case pinning
// that shape, which is how you keep discovering the fourth one in review.
//
// The invariant underneath all three: A FLAG AT THE END OF AN INVOCATION MUST
// SURVIVE, whatever appears before it. Asserting that across a cross-product of
// realistic value shapes tests the CLASS instead of the instances, and would have
// caught all three before review.

// docValueShapes are the token shapes real docs put in front of a flag. Add to
// this list rather than writing another single-case test.
var docValueShapes = []struct{ name, tok string }{
	{"placeholder", "<env>"},
	{"placeholder with slash", "<secret/path>"},
	{"placeholder with pipe", "<active|standby>"},
	{"two placeholders", "<owner>/<name>"},
	{"dotted plus version", "v1.33.6+lke7"},
	{"cidr", "203.0.113.0/24"},
	{"ipv6 cidr", "::/0"},
	{"node type", "g8-dedicated-8-4"},
	{"path", "~/.kube/lab.config"},
	{"obj cluster", "us-ord-10"},
	{"secret path", "secret/harbor/robot"},
	{"quoted with space", `"a b"`},
	{"equals form", "key=value"},
	{"duration", "30d"},
	{"url", "https://github.com/o/r.git"},
	{"branch ref", "apl-lab"},
	{"digits", "8"},
	{"dotted spec path", "cluster.nodePool.count=8"},
}

func TestInvocationTokens_TrailingFlagSurvivesEveryValueShape(t *testing.T) {
	for _, v := range docValueShapes {
		t.Run(v.name, func(t *testing.T) {
			// A value in the middle, and a flag after it that must be seen.
			rest := " env add lab --some-value " + v.tok + " --trailing-flag"
			got := invocationTokens(rest)
			var found bool
			for _, tok := range got {
				if tok == "--trailing-flag" {
					found = true
				}
			}
			if !found {
				t.Errorf("a flag after %s (%q) was LOST — the scan stopped early.\n  tokens: %q\n  this is the class that shipped three times; add the shape to docValueShapes and fix the tokeniser, do not special-case it here",
					v.name, v.tok, got)
			}
		})
	}
}

// The same invariant one level up: the guard must actually REPORT a bad flag that
// sits behind each shape. Tokenising correctly is necessary but not sufficient —
// the leading-flag defect tokenised fine and still resolved to no command.
func TestParseWorkflowDispatchInputs_AgreesWithRealWorkflows(t *testing.T) {
	root := repoRootForDocsGuard(t)
	var checked int
	for _, dir := range []string{
		filepath.Join(root, ".github", "workflows"),
		filepath.Join(root, "instance-template", ".github", "workflows"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
				continue
			}
			path := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			wi, err := parseWorkflowDispatchInputs(data)
			if err != nil {
				t.Errorf("%s: parse failed: %v", path, err)
				continue
			}
			checked++
			// The independent signal: the token appears in the raw `on:` BLOCK.
			// Textual, sharing no code with the YAML parser — but scoped, because
			// the first cut grepped the whole file and cried wolf on four
			// workflows that merely MENTION workflow_dispatch in a comment or an
			// `if:` condition. (Found by running it, not by reasoning about it.)
			naive := strings.Contains(rawOnBlock(string(data)), "workflow_dispatch")
			if naive != wi.dispatch {
				t.Errorf("%s: parser says dispatch=%v but the file %s the token — one of them is wrong (this is how the `on: [push, workflow_dispatch]` shorthand slipped through a hand-written fixture table)",
					filepath.Base(path), wi.dispatch,
					map[bool]string{true: "CONTAINS", false: "does NOT contain"}[naive])
			}
		}
	}
	if checked < 20 {
		t.Errorf("only %d workflow(s) cross-checked — the walk found too few to be meaningful", checked)
	}
}

// rawOnBlock returns the text of the top-level `on:` block — from the `on:` line
// to the next top-level key — without parsing YAML, so it stays an independent
// check on the parser rather than a second opinion from the same code.
func rawOnBlock(body string) string {
	lines := strings.Split(body, "\n")
	start := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "on:" || strings.HasPrefix(t, "on:") || strings.HasPrefix(t, `"on":`) || strings.HasPrefix(t, "'on':") {
			if l == strings.TrimLeft(l, " \t") { // top-level only
				start = i
				break
			}
		}
	}
	if start < 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(lines[start] + "\n")
	for _, l := range lines[start+1:] {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
			b.WriteString(l + "\n")
			continue
		}
		break // a new top-level key ends the block
	}
	return b.String()
}

// A leading `/` is ROOT-relative in Markdown — GitHub resolves it against the repo
// root, not the file's directory. Joining it to the file's dir turned a VALID link
// into `docs/docs/x.md` and reported it dead: a false positive, latent only because
// no doc uses that form today.
func TestCheckDocLinks_RootRelativeLinksResolveFromTheRoot(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "docs/target.md", "# target")
	writeMD(t, root, "instance-template/kubernetes-custom/README.md", "# custom")

	for _, tc := range []struct{ name, rel, body, wantHit string }{
		{
			// From a NESTED file, so joining to its dir would give
			// docs/runbooks/docs/target.md and wrongly report it.
			name: "root-relative from a nested file resolves",
			rel:  "docs/runbooks/r.md",
			body: "see [t](/docs/target.md)\n",
		},
		{
			name: "root-relative from a top-level file resolves",
			rel:  "README.md",
			body: "see [t](/docs/target.md)\n",
		},
		{
			// For a scaffold file the root is the INSTANCE root after render.
			name: "root-relative in instance-template resolves against the instance",
			rel:  "instance-template/AGENTS.md",
			body: "see [c](/kubernetes-custom/README.md)\n",
		},
		{
			name:    "a root-relative link to nothing is still reported",
			rel:     "docs/runbooks/r.md",
			body:    "see [x](/docs/nope.md)\n",
			wantHit: "does not exist",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeMD(t, root, tc.rel, tc.body)
			docs, bad := loadDocs(gRepo(root), []string{tc.rel})
			if len(bad) != 0 {
				t.Fatalf("fixture unreadable: %v", bad)
			}
			got := checkDocLinks(gRepo(root), docs, &Scanned{})
			if tc.wantHit == "" {
				if len(got) != 0 {
					t.Fatalf("a valid root-relative link was reported: %v", got)
				}
				return
			}
			if len(got) == 0 || !strings.Contains(got[0].Detail, tc.wantHit) {
				t.Fatalf("expected %q, got %v", tc.wantHit, got)
			}
		})
	}
}

// A TOC entry that no longer matches a heading is the whole reason the block is
// allowed to exist in a repo that otherwise refuses hand-maintained lists.
func TestCheckDocTOCs(t *testing.T) {
	cases := []struct {
		name, body string
		want       int
	}{
		{
			name: "every entry resolves",
			body: "# T\n\n<!-- toc -->\n## Contents\n\n- [Alpha](#alpha)\n- [Beta gamma](#beta-gamma)\n\n<!-- /toc -->\n\n## Alpha\n\n## Beta gamma\n",
		},
		{
			name: "renamed heading is caught",
			body: "# T\n\n<!-- toc -->\n- [Alpha](#alpha)\n<!-- /toc -->\n\n## Alpha renamed\n",
			want: 1,
		},
		{
			// The load-bearing case for this repo: headings here are full of
			// `workflow_call`, `promotion_rank`, `ha_role`. Dropping `_` from the
			// allowed set made every one of them a false positive.
			name: "underscores survive the slug",
			body: "# T\n\n<!-- toc -->\n- [`workflow_call` interface](#workflow_call-interface)\n<!-- /toc -->\n\n## `workflow_call` interface\n",
		},
		{
			name: "duplicate headings get GitHub's -1 suffix",
			body: "# T\n\n<!-- toc -->\n- [Notes](#notes)\n- [Notes](#notes-1)\n<!-- /toc -->\n\n## Notes\n\n## Notes\n",
		},
		{
			// Prose cross-references are NOT a claim to be exhaustive, so only the
			// delimited block is judged.
			name: "anchors outside the block are ignored",
			body: "# T\n\nSee [the missing part](#nowhere).\n\n## Alpha\n",
		},
		{
			name: "a heading inside a fence does not define an anchor",
			body: "# T\n\n<!-- toc -->\n- [Fake](#fake)\n<!-- /toc -->\n\n```\n## Fake\n```\n",
			want: 1,
		},
		{
			name: "no toc block at all is fine",
			body: "# T\n\n## Alpha\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n Scanned
			got := checkDocTOCs([]docFile{{rel: "d.md", body: tc.body}}, &n)
			if len(got) != tc.want {
				t.Fatalf("got %d finding(s), want %d: %v", len(got), tc.want, got)
			}
		})
	}
}

func TestGithubAnchor(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Topology", "topology"},
		{"`workflow_call` interface", "workflow_call-interface"},
		{"Writing / rotating secrets — dual-write", "writing--rotating-secrets--dual-write"},
		{"**Bold** and *italic*", "bold-and-italic"},
		{"A [linked](x.md) word", "a-linked-word"},
		{"Trailing punctuation!", "trailing-punctuation"},
		{"HA roles are declared, not hardcoded", "ha-roles-are-declared-not-hardcoded"},
	} {
		if got := githubAnchor(tc.in); got != tc.want {
			t.Errorf("githubAnchor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestGithubAnchor_MatchesGithubSlugger pins githubAnchor against the real
// implementation GitHub runs.
//
// testdata/github_slugs.json is every heading in this repo's Markdown, each
// paired with the slug `github-slugger` produced for it. It exists because the
// first cut of githubAnchor collapsed whitespace RUNS to one hyphen while
// github-slugger emits one hyphen PER SPACE — so " — " and " / " (which this
// repo's headings are full of) generated anchors that do not resolve on GitHub.
//
// That bug survived its own guard: the TOC generator used the same rule, so
// docs-guard compared a wrong anchor against a wrong anchor and passed. An
// ORACLE captured from the real implementation is the only thing that catches
// that class, which is why this fixture is checked in rather than hand-written.
//
// Regenerate with: node scratch/oracle.mjs (see the docs-authoring skill).
func TestGithubAnchor_MatchesGithubSlugger(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "github_slugs.json"))
	if err != nil {
		t.Skipf("oracle fixture unavailable: %v", err)
	}
	var cases []struct {
		Heading string `json:"heading"`
		Slug    string `json:"slug"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse oracle: %v", err)
	}
	if len(cases) < 500 {
		t.Fatalf("oracle has only %d heading(s) — it was regenerated over a shrunken scan", len(cases))
	}
	var bad int
	for _, c := range cases {
		// github-slugger de-duplicates within a document; the oracle resets per
		// heading, so every entry here is a FIRST occurrence and carries no
		// numeric suffix. docAnchors owns the suffixing separately.
		if got := githubAnchor(c.Heading); got != c.Slug {
			if bad++; bad <= 10 {
				t.Errorf("githubAnchor(%q)\n  got  %q\n  want %q", c.Heading, got, c.Slug)
			}
		}
	}
	if bad > 10 {
		t.Errorf("... and %d more mismatch(es)", bad-10)
	}
}

// A mermaid node label is a caption, not a shell line. The quickstart's
// lifecycle diagram has a node reading `llz doctor --env`, whose closing `"]`
// the tokeniser folded into the flag and reported as `--env]`.

// THE TREE ARRIVES AS DATA, NOT AS A PARENT, and this is what pins it.
//
// `llz ci gates` runs this guard PARENTLESS, where cmd.Root() is the command
// itself — a tree of one, against which 868 documented invocations resolved to
// nothing and were all skipped, reported clean. Parenting it to the real root is
// not the fix: cobra's Execute() on a parented command delegates to
// Root().ExecuteC() and recurses forever.
func TestDocsGuardCmdForUsesTheInjectedTree(t *testing.T) {
	tree := &cobra.Command{Use: "llz"}
	tree.AddCommand(&cobra.Command{Use: "render"})

	c := DocsGuardCmdFor(tree)
	if c.Root() == tree {
		t.Fatal("the command is PARENTED to the tree — that is what recurses; it must stay " +
			"parentless and hold the tree as a value")
	}
	if c.Root() != c {
		t.Errorf("expected a parentless command, got root %q", c.Root().Name())
	}
	// The plain constructor keeps the live-CLI behaviour: no tree, falls back to
	// cmd.Root() at run time.
	if DocsGuardCmd() == nil {
		t.Error("DocsGuardCmd() returned nil")
	}
}

// gRepo is the reader a real run gets, fenced to a fixture tree. Built from the
// EXTENSION so a test cannot hand itself a reader the declaration would not have
// produced — the rule capability.WithExec follows for the same reason.
func gRepo(root string) capability.Repo {
	return capability.RepoForGate(Extension(), root)
}
