package docsguard

// workflowruns_test.go — the workflow corpus: `run:` extraction and shell
// visibility.
//
// THE GAP THESE CLOSE cost an e2e round. `llz-terraform.yml` invoked
// `llz ci preflight --deployment` after an extraction dropped that flag, and the
// branch went green on all eleven CI checks because docs-guard validated
// Markdown only. The invocations that actually RUN were the ones nobody checked.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkflowRunBodyKeepsOriginalLineNumbers is the property a finding's
// usefulness rests on: report the wrong line and the reader is sent to unrelated
// YAML in a thousand-line workflow.
func TestWorkflowRunBodyKeepsOriginalLineNumbers(t *testing.T) {
	src := "name: x\n" + // 1
		"jobs:\n" + // 2
		"  a:\n" + // 3
		"    steps:\n" + // 4
		"      - run: |\n" + // 5
		"          echo one\n" + // 6
		"          llz ci preflight --deployment e2e\n" // 7
	body, err := workflowRunBody([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(body, "\n")
	if got := strings.TrimSpace(lines[6]); got != "llz ci preflight --deployment e2e" {
		t.Errorf("line 7 (index 6) should hold the invocation, got %q", got)
	}
	// Everything that is not a run: scalar must be blank, or the guard is back to
	// scanning comments and prose.
	for _, i := range []int{0, 1, 2, 3} {
		if strings.TrimSpace(lines[i]) != "" {
			t.Errorf("line %d should be blank, got %q", i+1, lines[i])
		}
	}
}

// TestWorkflowRunBodyIgnoresComments pins the decision to scan `run:` only. A
// YAML comment naming a flag is documentation — often deliberately historical —
// and reporting it would make the guard a nuisance and get it switched off.
func TestWorkflowRunBodyIgnoresComments(t *testing.T) {
	src := "jobs:\n  a:\n    steps:\n" +
		"      # Same resolver `llz ci preflight --deployment` uses.\n" +
		"      - run: echo hi\n"
	body, err := workflowRunBody([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "preflight") {
		t.Errorf("a comment must not enter the scanned body:\n%s", body)
	}
}

// TestWorkflowRunBodyFailsOnUnparseableYAML — fail closed. A workflow the guard
// cannot parse is one it cannot vouch for.
func TestWorkflowRunBodyFailsOnUnparseableYAML(t *testing.T) {
	if _, err := workflowRunBody([]byte("jobs:\n  a:\n   - broken\n  b: [unclosed\n")); err == nil {
		t.Fatal("unparseable YAML must be an error, not an empty body that scans clean")
	}
}

// TestShellVisible covers both noise classes the first cut produced, and the
// invocation that must survive them.
func TestShellVisible(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		wantHas  []string
		wantNot  []string
	}{{
		name:    "command substitution loses its parens",
		in:      `BUCKETS=$(llz ci tf-output bucket_names --json)`,
		wantHas: []string{"llz ci tf-output bucket_names --json"},
		wantNot: []string{"--json)"},
	}, {
		// `--yes` is a real global flag; it was reported only because the scan ran
		// past the closing quote and swallowed "(or llz" as a flag name.
		name:    "prose inside a quoted echo is not an invocation",
		in:      `echo "applies run on dispatch, which 'llz build <env> --yes' (or 'llz up <env> --yes') fires."`,
		wantNot: []string{"llz build", "llz up"},
	}, {
		name:    "a real invocation with quoted values keeps its flag names",
		in:      `llz ci preflight --deployment "$REGION" --fail-on-orphans "$FAIL_ON_ORPHANS"`,
		wantHas: []string{"--deployment", "--fail-on-orphans", "llz ci preflight"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := shellVisible(tc.in)
			if len(got) != len(tc.in) {
				t.Errorf("length must be preserved: %d != %d", len(got), len(tc.in))
			}
			for _, w := range tc.wantHas {
				if !strings.Contains(got, w) {
					t.Errorf("want %q in %q", w, got)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("did NOT want %q in %q", w, got)
				}
			}
		})
	}
}

// writeWF materialises one workflow file under root.
func writeWF(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWorkflowYAMLFilesCoversBothTreesAndSkipsArtifacts pins the corpus. The
// instance-template half is the one that matters — those workflows are DELIVERED,
// so a wrong flag there breaks every adopter rather than only this repo.
func TestWorkflowYAMLFilesCoversBothTreesAndSkipsArtifacts(t *testing.T) {
	root := t.TempDir()
	writeWF(t, root, ".github/workflows/lint.yml", "on: push\n")
	writeWF(t, root, ".github/actions/setup-llz/action.yml", "name: x\n")
	writeWF(t, root, "instance-template/.github/workflows/llz-terraform.yml", "on: push\n")
	writeWF(t, root, "instance-template/.github/workflows/note.yaml", "on: push\n")
	writeWF(t, root, ".github/workflows/README.md", "not yaml")
	// Artifact trees carry a rendered copy of the same workflows; scanning them
	// double-reports every finding against a tree nobody edits.
	writeWF(t, root, ".e2e-instance/.github/workflows/copy.yml", "on: push\n")

	got, err := workflowYAMLFiles(gRepo(root))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		".github/workflows/lint.yml":                            true,
		".github/actions/setup-llz/action.yml":                  true,
		"instance-template/.github/workflows/llz-terraform.yml": true,
		"instance-template/.github/workflows/note.yaml":         true,
	}
	if len(got) != len(want) {
		t.Fatalf("want %d files, got %d: %v", len(want), len(got), got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected file in corpus: %s", g)
		}
	}
}

// TestWorkflowYAMLFilesToleratesAMissingRoot — the gate also runs in an INSTANCE
// checkout, which has no instance-template/ of its own. That is not a failure.
func TestWorkflowYAMLFilesToleratesAMissingRoot(t *testing.T) {
	root := t.TempDir()
	writeWF(t, root, ".github/workflows/lint.yml", "on: push\n")
	got, err := workflowYAMLFiles(gRepo(root))
	if err != nil {
		t.Fatalf("a missing instance-template/ must not fail the walk: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("want just the one workflow, got %v", got)
	}
}

// TestLoadWorkflowRunsReportsUnparseable — fail closed, with the file named.
func TestLoadWorkflowRunsReportsUnparseable(t *testing.T) {
	root := t.TempDir()
	writeWF(t, root, ".github/workflows/ok.yml", "jobs:\n  a:\n    steps:\n      - run: llz version\n")
	writeWF(t, root, ".github/workflows/bad.yml", "jobs:\n  a: [unclosed\n")

	docs, bad := loadWorkflowRuns(gRepo(root), []string{
		".github/workflows/bad.yml", ".github/workflows/ok.yml",
	})
	if len(docs) != 1 {
		t.Errorf("the readable workflow should still be scanned, got %d", len(docs))
	}
	if len(bad) != 1 || bad[0].Kind != "unparseable" {
		t.Fatalf("want one unparseable finding, got %+v", bad)
	}
	if !strings.Contains(bad[0].File, "bad.yml") {
		t.Errorf("the finding must name the file, got %q", bad[0].File)
	}
}

// TestLoadWorkflowRunsReportsUnreadable — a workflow the guard cannot read must
// not be silently dropped from a corpus it then reports as clean.
func TestLoadWorkflowRunsReportsUnreadable(t *testing.T) {
	root := t.TempDir()
	_, bad := loadWorkflowRuns(gRepo(root), []string{".github/workflows/gone.yml"})
	if len(bad) != 1 || bad[0].Kind != "unreadable" {
		t.Fatalf("want one unreadable finding, got %+v", bad)
	}
}
