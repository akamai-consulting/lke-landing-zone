package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ddWriteDocs materialises a docs/ tree and returns its root.
func ddWriteDocs(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func ddRead(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestPlural pins the agreement in the delivery summary line ("1 entry" vs
// "2 entries") — the only human-readable record of how much a delivery pruned.
func TestPlural(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{0, "ies"}, {1, "y"}, {2, "ies"}, {27, "ies"}} {
		if got := plural(tc.n, "y", "ies"); got != tc.want {
			t.Errorf("plural(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestDeliverDocsKeepsLocalLinksRelative pins the "present" set the link rewrite
// is built from. If it is empty or holds the wrong entries, links BETWEEN the
// kept docs get rewritten to the template URL — an operator following
// quickstart → runbook leaves the instance for GitHub, and every one of those
// links then churns on each delivery. Also pins that --org reaches the rewritten
// links (a fork's docs must not point at akamai-consulting).
func TestDeliverDocsKeepsLocalLinksRelative(t *testing.T) {
	dir := ddWriteDocs(t, map[string]string{
		"quickstart.md":       "Recover with [the runbook](runbooks/recover.md); see [secrets](secrets.md).\n",
		"runbooks/recover.md": "Back to [quickstart](../quickstart.md).\n",
		"playbooks/rotate.md": "Rotate; see [the guide](../adopter-guide.md).\n",
		"secrets.md":          "referenced\n",
		"adopter-guide.md":    "referenced\n",
	})
	if err := runDeliverDocs(dir, "myorg", "v1.2.3", "", ""); err != nil {
		t.Fatalf("runDeliverDocs: %v", err)
	}

	q := ddRead(t, dir, "quickstart.md")
	if !strings.Contains(q, "](runbooks/recover.md)") {
		t.Errorf("a link to a still-delivered doc must stay relative:\n%s", q)
	}
	if !strings.Contains(q, "](https://github.com/myorg/lke-landing-zone/blob/main/docs/secrets.md)") {
		t.Errorf("a link to a referenced doc must repoint at --org's template repo:\n%s", q)
	}
	// A kept doc in a SUBDIR linking up to another kept doc resolves relative to
	// docs/, so it too must stay local.
	if r := ddRead(t, dir, "runbooks/recover.md"); !strings.Contains(r, "](../quickstart.md)") {
		t.Errorf("subdir → kept-doc link must stay relative:\n%s", r)
	}
	if p := ddRead(t, dir, "playbooks/rotate.md"); !strings.Contains(p, "](https://github.com/myorg/lke-landing-zone/blob/main/docs/adopter-guide.md)") {
		t.Errorf("subdir → referenced-doc link must repoint (resolved against docs/):\n%s", p)
	}
}

// The org default applies only when --org is empty.
func TestDeliverDocsDefaultsOrgOnlyWhenUnset(t *testing.T) {
	dir := ddWriteDocs(t, map[string]string{
		"quickstart.md": "See [secrets](secrets.md).\n",
		"secrets.md":    "referenced\n",
	})
	if err := runDeliverDocs(dir, "", "v1.2.3", "", ""); err != nil {
		t.Fatalf("runDeliverDocs: %v", err)
	}
	if q := ddRead(t, dir, "quickstart.md"); !strings.Contains(q, "github.com/akamai-consulting/lke-landing-zone/blob/main/docs/secrets.md") {
		t.Errorf("empty --org must fall back to akamai-consulting:\n%s", q)
	}
}
