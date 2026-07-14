package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDeliverDocs(t *testing.T) {
	dir := t.TempDir()
	// A representative docs/ tree.
	write := func(p, c string) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("quickstart.md", "keep")
	write("runbooks/recover.md", "keep")
	write("playbooks/rotate.md", "keep")
	write("secrets.md", "reference")
	write("adopter-guide.md", "reference")
	write("designs/reconciler.md", "reference")
	write("architecture/windows.md", "reference")

	if err := runDeliverDocs(dir, "myorg", "v1.2.3"); err != nil {
		t.Fatalf("runDeliverDocs: %v", err)
	}

	// Kept.
	for _, p := range []string{"quickstart.md", "runbooks/recover.md", "playbooks/rotate.md"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("keep-set entry pruned: %s", p)
		}
	}
	// Referenced (pruned).
	for _, p := range []string{"secrets.md", "adopter-guide.md", "designs", "architecture"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be pruned (referenced)", p)
		}
	}
	// Pointer written, version-pinned.
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("no README.md pointer written: %v", err)
	}
	if !strings.Contains(string(readme), "github.com/myorg/lke-landing-zone/tree/v1.2.3/docs") {
		t.Errorf("pointer missing the version-pinned URL:\n%s", readme)
	}

	// Idempotent — a second run over the already-pruned tree is a no-op success.
	if err := runDeliverDocs(dir, "myorg", "v1.2.3"); err != nil {
		t.Errorf("second run failed (not idempotent): %v", err)
	}
}

func TestRewriteDocLinks(t *testing.T) {
	present := map[string]bool{
		"quickstart.md":         true,
		"runbooks/bootstrap.md": true,
		"playbooks/rotate.md":   true,
	}
	// A file at docs/quickstart.md linking to kept + referenced docs.
	in := "See [secrets](secrets.md), [a runbook](runbooks/bootstrap.md#step), " +
		"[design](designs/reconciler.md), [arch](../docs/x.md), " +
		"[home](https://example.com), [anchor](#top)."
	out := rewriteDocLinks(in, "", present, "myorg", "v9")

	// Referenced .md → template URL (anchor preserved).
	if !strings.Contains(out, "](https://github.com/myorg/lke-landing-zone/blob/v9/docs/secrets.md)") {
		t.Errorf("secrets.md not repointed:\n%s", out)
	}
	if !strings.Contains(out, "docs/designs/reconciler.md)") {
		t.Errorf("designs link not repointed:\n%s", out)
	}
	// Kept doc → stays relative (with anchor).
	if !strings.Contains(out, "](runbooks/bootstrap.md#step)") {
		t.Errorf("kept-doc link should stay relative:\n%s", out)
	}
	// External + pure-anchor untouched.
	if !strings.Contains(out, "](https://example.com)") || !strings.Contains(out, "](#top)") {
		t.Errorf("external/anchor links altered:\n%s", out)
	}
}

func TestDocsPointerDefaults(t *testing.T) {
	p := docsPointer("", "")
	if !strings.Contains(p, "github.com/akamai-consulting/lke-landing-zone/tree/main/docs") {
		t.Errorf("default pointer wrong:\n%s", p)
	}
}
