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
	out := rewriteDocLinks(in, "", present, "myorg")

	// Referenced .md → template URL at main (NOT the instance's pinned version —
	// pinning these is what made every upgrade churn every kept doc).
	if !strings.Contains(out, "](https://github.com/myorg/lke-landing-zone/blob/main/docs/secrets.md)") {
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

// The pinned tree pointer and the unpinned cross-doc links are the whole point of
// the split: an upgrade must move docs/README.md and NOTHING else under docs/.
func TestDeliverDocsPinsOnlyThePointer(t *testing.T) {
	dir := t.TempDir()
	write := func(p, c string) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("quickstart.md", "See [secrets](secrets.md) and [alerting](alerting.md).")
	write("runbooks/recover.md", "See [the guide](../adopter-guide.md).")
	write("secrets.md", "reference")
	write("alerting.md", "reference")
	write("adopter-guide.md", "reference")

	if err := runDeliverDocs(dir, "myorg", "v1.2.3"); err != nil {
		t.Fatalf("runDeliverDocs: %v", err)
	}
	snapshot := func(p string) string {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return string(b)
	}
	kept := []string{"quickstart.md", "runbooks/recover.md"}
	before := map[string]string{}
	for _, p := range kept {
		before[p] = snapshot(p)
		if strings.Contains(before[p], "v1.2.3") {
			t.Errorf("%s carries the version pin — it will churn on every upgrade:\n%s", p, before[p])
		}
		if !strings.Contains(before[p], "/blob/main/docs/") {
			t.Errorf("%s links not repointed to the template:\n%s", p, before[p])
		}
	}
	if !strings.Contains(snapshot("README.md"), "tree/v1.2.3/docs") {
		t.Error("the pointer must stay pinned to the instance's template release")
	}

	// Re-deliver at the NEXT version: only the pointer moves.
	if err := runDeliverDocs(dir, "myorg", "v1.2.4"); err != nil {
		t.Fatalf("re-deliver: %v", err)
	}
	for _, p := range kept {
		if got := snapshot(p); got != before[p] {
			t.Errorf("%s changed across a version bump:\n--- before\n%s\n--- after\n%s", p, before[p], got)
		}
	}
	if !strings.Contains(snapshot("README.md"), "tree/v1.2.4/docs") {
		t.Error("the pointer did not re-pin to the new version")
	}
}

// An instance delivered by an older llz carries cross-doc links pinned to whatever
// release was current then. Those are absolute, so the relative-link rewrite skips
// them and they stay stale forever (gsap-apl: 27 of them, one release behind its own
// pointer). Re-delivery must heal them.
func TestDeliverDocsHealsPermalinksLeftPinnedByAnOlderDelivery(t *testing.T) {
	dir := t.TempDir()
	write := func(p, c string) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("quickstart.md",
		"See [secrets](https://github.com/myorg/lke-landing-zone/blob/v0.0.32/docs/secrets.md).\n")
	write("runbooks/recover.md",
		"See [the guide](https://github.com/myorg/lke-landing-zone/blob/v0.0.32/docs/adopter-guide.md#6-bootstrap-order).\n")

	if err := runDeliverDocs(dir, "myorg", "v0.0.33"); err != nil {
		t.Fatalf("runDeliverDocs: %v", err)
	}
	for _, p := range []string{"quickstart.md", "runbooks/recover.md"} {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		got := string(b)
		if strings.Contains(got, "/blob/v0.0.32/") {
			t.Errorf("%s kept the stale pin: %s", p, got)
		}
		if !strings.Contains(got, "/blob/main/docs/") {
			t.Errorf("%s was not healed to the tracking branch: %s", p, got)
		}
	}
	// Healing must not smuggle the NEW version into the links either.
	b, _ := os.ReadFile(filepath.Join(dir, "quickstart.md"))
	if strings.Contains(string(b), "v0.0.33") {
		t.Errorf("healing re-pinned the link instead of floating it: %s", b)
	}
}
