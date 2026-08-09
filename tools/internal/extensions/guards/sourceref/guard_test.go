package sourceref

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// writeTree materialises a repo from a path->content map and returns its root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// ── the declaration ─────────────────────────────────────────────────────────

// A gate binding may hold read-repo and nothing else; Validate() enforces it, but
// this pins the declaration itself so a widening is caught where it is written.
func TestExtensionIsAGateHoldingOnlyReadRepo(t *testing.T) {
	e := Extension()
	if errs := extension.ValidateSet([]extension.Extension{e}); len(errs) > 0 {
		t.Fatalf("declaration must validate: %v", errs)
	}
	if len(e.Bindings) != 1 {
		t.Fatalf("expected exactly one binding, got %d", len(e.Bindings))
	}
	b := e.Bindings[0]
	if b.Kind != extension.Gate {
		t.Errorf("kind should be gate, got %q", b.Kind)
	}
	if len(b.Grants) != 1 || b.Grants[0] != extension.ReadRepo {
		t.Errorf("a gate reads the repo and nothing else; got %v", b.Grants)
	}
}

func TestSourceRefBindingIsTheDeclaredGate(t *testing.T) {
	if got := sourceRefBinding(); got.Kind != extension.Gate {
		t.Errorf("the reader must be built from the gate binding, got %q", got.Kind)
	}
}

// ── extraction ──────────────────────────────────────────────────────────────

func TestExtractRefsFindsPathsInEveryProseShape(t *testing.T) {
	body := strings.Join([]string{
		"see `tools/internal/cli/commands.go` for the wiring",       // markdown backticks
		"# tools/internal/shared/openbao/openbao.go owns baoEnv",    // yaml/shell comment
		`   fmt.Errorf("register it in tools/x/y.go with the key")`, // go string literal
		"nothing on this line",
	}, "\n")

	got := extractRefs("f.md", body)
	want := []string{
		"tools/internal/cli/commands.go",
		"tools/internal/shared/openbao/openbao.go",
		"tools/x/y.go",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d refs, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i].Ref != w {
			t.Errorf("ref %d: want %q, got %q", i, w, got[i].Ref)
		}
		if got[i].Line != i+1 {
			t.Errorf("ref %d: want line %d, got %d", i, i+1, got[i].Line)
		}
	}
}

// Prose punctuation must not become part of the path, or every sentence-final
// reference reports as broken and the guard is unusable.
func TestExtractRefsTrimsTrailingProsePunctuation(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"it lives in tools/internal/cli.", "tools/internal/cli"},
		{"see tools/internal/cli/commands.go.", "tools/internal/cli/commands.go"},
		{"under tools/internal/extensions/guards/", "tools/internal/extensions/guards"},
	} {
		got := extractRefs("f.md", tc.in)
		if len(got) != 1 || got[0].Ref != tc.want {
			t.Errorf("%q: want %q, got %v", tc.in, tc.want, got)
		}
	}
}

// A paragraph that wraps mid-filename leaves a trailing hyphen. An earlier draft
// trimmed it alongside `.` and `/`, which turned half a path into a confident
// claim about a file nobody wrote — `tools/internal/shared/team-scoped-` reported
// as missing while `team-scoped-credentials.md` sat there. Skipped instead: a
// wrapped path is unverifiable from one line, and inventing the missing half is
// worse than saying nothing.
func TestExtractRefsSkipsAPathWrappedMidHyphen(t *testing.T) {
	for _, in := range []string{
		"the source is tools/internal/shared/team-scoped-",
		"tools/cmd/llz-",
	} {
		if got := extractRefs("f.md", in); len(got) != 0 {
			t.Errorf("%q: a wrapped path must be skipped, got %v", in, got)
		}
	}
}

// A placeholder is not a path. These truncate at the metacharacter to their real
// directory prefix, which resolves — reporting them would flag the docs that
// teach the layout, and the only fix would be an ignore-list.
func TestExtractRefsTruncatesPlaceholdersToTheirRealPrefix(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"extract to tools/internal/<pkg> (ADR 0013)", "tools/internal"},
		{"grouped tools/internal/extensions/{assertions,guards}/", "tools/internal/extensions"},
	} {
		got := extractRefs("f.md", tc.in)
		if len(got) != 1 || got[0].Ref != tc.want {
			t.Errorf("%q: want %q, got %v", tc.in, tc.want, got)
		}
	}
}

func TestExtractRefsSkipsRecursiveGlobs(t *testing.T) {
	if got := extractRefs("f.sh", "unit tests (tools/internal/**/*_test.go)"); len(got) != 0 {
		t.Errorf("a `**` pattern names a shape, not a place; got %v", got)
	}
}

// ── resolution ──────────────────────────────────────────────────────────────

func TestRunPassesWhenEveryReferenceResolves(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/internal/cli/commands.go": "package cli\n",
		"docs/a.md":                      "the wiring is in `tools/internal/cli/commands.go`.\n",
	})
	if err := Run(root); err != nil {
		t.Fatalf("every reference resolves, so the gate must pass: %v", err)
	}
}

func TestRunFailsOnAStaleReference(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/internal/cli/commands.go": "package cli\n",
		"docs/a.md":                      "see `tools/cmd/llz/ci_bao_seed_all.go` — add new seeds THERE.\n",
	})
	err := Run(root)
	if err == nil {
		t.Fatal("a reference to a file that does not exist must fail the gate")
	}
	if !strings.Contains(err.Error(), "1 stale path reference") {
		t.Errorf("the error should count the findings, got %v", err)
	}
}

// The defect class that motivated the guard: a path that is stale inside a YAML
// comment, where no structured parser looks.
func TestRunFindsStalePathsInYAMLAndShellComments(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/keep.go":  "package keep\n",
		"chart/v.yaml":   "key: 1 # see tools/cmd/llz/openbao.go\n",
		"scripts/lib.sh": "# keep in step with tools/cmd/llz/scaffold.go\n",
	})
	err := Run(root)
	if err == nil {
		t.Fatal("stale paths in comments must fail")
	}
	if !strings.Contains(err.Error(), "2 stale path reference") {
		t.Errorf("both comments should be found, got %v", err)
	}
}

// A glob that has quietly stopped matching is the same defect as a missing file —
// and is the one that produces a spectacular-looking green.
func TestRunFailsOnAGlobThatMatchesNothing(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/internal/cli/commands.go": "package cli\n",
		"budget.yaml":                    "include:\n  - \"tools/internal/cli/*.json\"\n",
	})
	if err := Run(root); err == nil {
		t.Fatal("a glob matching zero files must fail, not pass silently")
	}
}

func TestRunPassesAGlobThatMatches(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/internal/cli/commands.go": "package cli\n",
		"budget.yaml":                    "include:\n  - \"tools/internal/cli/*.go\"\n",
	})
	if err := Run(root); err != nil {
		t.Fatalf("a glob with a match must pass: %v", err)
	}
}

func TestRunResolvesDirectoryReferences(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/internal/shared/openbao/openbao.go": "package openbao\n",
		"docs/a.md": "the package is `tools/internal/shared/openbao/`.\n",
	})
	if err := Run(root); err != nil {
		t.Fatalf("a directory reference must resolve: %v", err)
	}
}

// ── fail-closed arms ────────────────────────────────────────────────────────

// The classic: pointed at a tree with nothing to read, the guard must refuse
// rather than report that everything is fine.
func TestRunFailsOnAnEmptyCorpus(t *testing.T) {
	err := Run(writeTree(t, map[string]string{}))
	if err == nil {
		t.Fatal("a guard that examined nothing must not report green")
	}
	if !strings.Contains(err.Error(), "examined nothing") {
		t.Errorf("the error should say the corpus was empty, got %v", err)
	}
}

// The subtler one, and the arm no unit test of extractRefs can cover: a corpus
// was found and read, but not one reference came out of it. In this repo that
// means the extraction is broken, not that the docs are path-free.
func TestRunFailsWhenTheCorpusYieldsNoReferences(t *testing.T) {
	root := writeTree(t, map[string]string{
		"docs/a.md":    "prose that names no path at all\n",
		"scripts/b.sh": "echo hello\n",
	})
	err := Run(root)
	if err == nil {
		t.Fatal("files scanned but zero references extracted must fail")
	}
	if !strings.Contains(err.Error(), "not one") {
		t.Errorf("the error should say no reference was extracted, got %v", err)
	}
}

// Test files legitimately contain broken paths — this file does. Scanning them
// would make the guard fail on its own fixtures.
func TestScannableExcludesTestFilesAndUnknownTypes(t *testing.T) {
	for name, want := range map[string]bool{
		"guard.go":      true,
		"guard_test.go": false,
		"README.md":     true,
		"values.yaml":   true,
		"lib.sh":        true,
		"Makefile":      true,
		"image.png":     false,
		"go.sum":        false,
	} {
		if got := scannable(name); got != want {
			t.Errorf("scannable(%q) = %v, want %v", name, got, want)
		}
	}
}

// Generated and third-party trees carry paths this repo does not own; walking
// them reports another checkout's findings against this one.
func TestRunSkipsGeneratedAndVendoredTrees(t *testing.T) {
	root := writeTree(t, map[string]string{
		"tools/keep.go":                "package keep\n",
		"docs/a.md":                    "see `tools/keep.go`.\n",
		".instance-test/instance/x.md": "see `tools/cmd/llz/gone.go`.\n",
		"node_modules/pkg/README.md":   "see `tools/cmd/llz/also-gone.go`.\n",
	})
	if err := Run(root); err != nil {
		t.Fatalf("findings inside generated trees must not fail this repo: %v", err)
	}
}

// repoFor builds the read-repo handle the guards use, for tests that exercise a
// helper below Run rather than Run itself.
func repoFor(t *testing.T, root string) capability.Repo {
	t.Helper()
	return capability.RepoForGate(Extension(), root)
}
