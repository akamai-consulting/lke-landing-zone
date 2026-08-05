package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The whole design rests on one property: change the SOURCE, and --check goes red.
// Everything else is plumbing. This asserts it against the real registry by
// rendering a block, then rendering it again with a doctored source value.
func TestDocsFacts_CheckFailsWhenTheSourceChanges(t *testing.T) {
	root := t.TempDir()
	rootCmd := newRootCmd()

	f, ok := factByName("openbao.in-pod-env")
	if !ok {
		t.Fatal("openbao.in-pod-env missing from the registry")
	}
	// A doc holding the CURRENT rendering passes.
	writeMD(t, root, "docs/d.md", renderFactBlock(f.name, f.render(rootCmd))+"\n")
	if err := runDocsFactsForTest(t, root, true, rootCmd); err != nil {
		t.Fatalf("a freshly rendered block must pass --check: %v", err)
	}

	// A doc holding a STALE rendering fails, and says which fact and where the
	// truth lives — the two things a reader needs to fix it.
	writeMD(t, root, "docs/d.md", renderFactBlock(f.name, "BAO_ADDR=https://127.0.0.1:8200")+"\n")
	err := runDocsFactsForTest(t, root, true, rootCmd)
	if err == nil {
		t.Fatal("a stale block must fail --check — this is the entire point of the command")
	}
	if !strings.Contains(err.Error(), "drifted") {
		t.Errorf("error = %q, want it to name the drift", err)
	}
}

// --check must not write. A checker that repairs the thing it is checking turns a
// red build green on the next run and hides the drift.
func TestDocsFacts_CheckWritesNothing(t *testing.T) {
	root := t.TempDir()
	stale := renderFactBlock("openbao.in-pod-env", "BAO_ADDR=wrong") + "\n"
	writeMD(t, root, "docs/d.md", stale)
	_ = runDocsFactsForTest(t, root, true, newRootCmd())
	got, err := os.ReadFile(filepath.Join(root, "docs", "d.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != stale {
		t.Errorf("--check rewrote the file:\n got %q\nwant %q", got, stale)
	}
}

// Write mode repairs a stale block in place and leaves the surrounding prose —
// which is hand-written and is not this command's business — untouched.
func TestDocsFacts_WriteRepairsOnlyTheBlock(t *testing.T) {
	root := t.TempDir()
	rootCmd := newRootCmd()
	before := "# Title\n\nProse that explains WHY, which no generator should own.\n\n" +
		renderFactBlock("loki.tenants", "stale content") + "\n\nMore prose.\n"
	writeMD(t, root, "docs/d.md", before)
	if err := runDocsFactsForTest(t, root, false, rootCmd); err != nil {
		t.Fatalf("write mode: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "docs", "d.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Title",
		"Prose that explains WHY, which no generator should own.",
		"More prose.",
		defaultCollectorTenant, // the fact is now rendered from source
		defaultAuditTenant,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q after regeneration:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "stale content") {
		t.Errorf("the stale payload survived:\n%s", got)
	}
	// Idempotent: a second run changes nothing.
	if err := runDocsFactsForTest(t, root, true, rootCmd); err != nil {
		t.Errorf("regeneration is not idempotent — --check failed right after a write: %v", err)
	}
}

// A fact nobody renders rots unnoticed — the same shape of silent decay this
// command exists to prevent, so it is an error rather than a shrug.
func TestDocsFacts_OrphanedFactIsReported(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "docs/d.md", "# nothing rendered here\n")
	var err error
	captureStdout(t, func() { err = runDocsFacts(root, true, true, newRootCmd()) })
	if err == nil {
		t.Fatal("facts registered but rendered nowhere must be reported")
	}
	if !strings.Contains(err.Error(), "drifted") {
		t.Errorf("error = %q, want the orphan surfaced", err)
	}
}

// An unknown fact name is a typo in a doc, and must be named rather than ignored.
func TestDocsFacts_UnknownFactIsReported(t *testing.T) {
	root := t.TempDir()
	writeMD(t, root, "docs/d.md", "<!-- llz:fact openbao.no-such-thing -->\n```text\nx\n```\n<!-- /llz:fact -->\n")
	err := runDocsFactsForTest(t, root, true, newRootCmd())
	if err == nil {
		t.Fatal("an unknown fact name must be reported")
	}
}

// Every registered fact must render something, and must not carry a trailing
// newline into the block (which would make regeneration non-idempotent).
func TestDocsFacts_RegistryRendersCleanly(t *testing.T) {
	rootCmd := newRootCmd()
	for _, f := range docFacts {
		t.Run(f.name, func(t *testing.T) {
			got := f.render(rootCmd)
			if strings.TrimSpace(got) == "" {
				t.Fatal("renders empty — the source is probably unresolved")
			}
			if strings.HasSuffix(got, "\n") {
				t.Error("renders a trailing newline; renderFactBlock adds its own")
			}
			// A fact whose source could not be resolved renders a marker rather
			// than failing loudly — catch that here instead of shipping it.
			for _, bad := range []string{"(unresolved command", "(no --"} {
				if strings.Contains(got, bad) {
					t.Errorf("source did not resolve: %q", got)
				}
			}
			if f.source == "" || f.what == "" {
				t.Error("every fact needs `what` and `source` — --list is how a reader finds the truth")
			}
		})
	}
}

// The repo's own docs must be current, so a source change that nobody
// regenerated fails here as well as in CI.
func TestDocsFacts_ThisRepoIsCurrent(t *testing.T) {
	root := repoRootForDocsGuard(t)
	var err error
	captureStdout(t, func() { err = runDocsFacts(root, true, true, newRootCmd()) })
	if err != nil {
		t.Errorf("%v\n\nrun `make docs-facts` to regenerate", err)
	}
}

// runDocsFactsForTest silences the command's stdout chatter so a failing
// assertion is readable.
func runDocsFactsForTest(t *testing.T, root string, check bool, _ *cobra.Command) error {
	t.Helper()
	var err error
	captureStdout(t, func() { err = runDocsFacts(root, check, false, newRootCmd()) })
	return err
}
