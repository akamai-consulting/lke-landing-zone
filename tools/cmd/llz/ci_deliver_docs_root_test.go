package main

// Tests for the instance-ROOT link repoint — the half repointReferencedLinks
// never covered, which is why AGENTS.md → docs/adopter-guide.md shipped dead in
// every rendered instance.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tmpl/inst build the two lookups rewriteInstanceRootLinks takes, from plain
// sets — so each case states exactly which side a path lives on.
func lookups(instance, template, dirs []string) (func(string) bool, func(string) (bool, bool)) {
	in := map[string]bool{}
	for _, p := range instance {
		in[p] = true
	}
	tm := map[string]bool{}
	for _, p := range template {
		tm[p] = true
	}
	dir := map[string]bool{}
	for _, p := range dirs {
		dir[p] = true
	}
	return func(p string) bool { return in[p] },
		func(p string) (bool, bool) { return tm[p], dir[p] }
}

func TestRewriteInstanceRootLinks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		content  string
		fileDir  string
		instance []string
		template []string
		dirs     []string
		want     string
		wantN    int
	}{
		{
			// The shipped bug: deliver-docs prunes adopter-guide.md, AGENTS.md
			// still links to it, and the docs/-scoped rewrite never looks here.
			name:     "pruned doc linked from the instance root is repointed",
			content:  "see [the guide](docs/adopter-guide.md) for more",
			instance: []string{"docs/quickstart.md"},
			template: []string{"docs/adopter-guide.md"},
			want:     "see [the guide](https://github.com/myorg/lke-landing-zone/blob/main/docs/adopter-guide.md) for more",
			wantN:    1,
		},
		{
			// A template-only TREE gets /tree/, not /blob/.
			name:     "template-only directory gets a tree URL",
			content:  "manifests live in [`../../platform-apl/`](../../platform-apl/)",
			fileDir:  "apl-values/nested",
			template: []string{"platform-apl"},
			dirs:     []string{"platform-apl"},
			want:     "manifests live in [`../../platform-apl/`](https://github.com/myorg/lke-landing-zone/tree/main/platform-apl)",
			wantN:    1,
		},
		{
			// THE load-bearing case. landingzone.yaml is written by the first
			// `llz env add`, so it is absent from the instance AND from the
			// template (which ships only the .example). Repointing it would
			// mint a 404; it must be left exactly as-is.
			name:     "absent from both is left alone, not repointed to a 404",
			content:  "edit [`landingzone.yaml`](landingzone.yaml)",
			instance: []string{"landingzone.yaml.example"},
			template: []string{"landingzone.yaml.example"},
			want:     "edit [`landingzone.yaml`](landingzone.yaml)",
			wantN:    0,
		},
		{
			name:     "a link that resolves in the instance stays relative",
			content:  "start at [quickstart](docs/quickstart.md)",
			instance: []string{"docs/quickstart.md"},
			template: []string{"docs/quickstart.md"},
			want:     "start at [quickstart](docs/quickstart.md)",
			wantN:    0,
		},
		{
			name:     "anchors survive the rewrite",
			content:  "[§5](docs/adopter-guide.md#5-org-literals)",
			template: []string{"docs/adopter-guide.md"},
			want:     "[§5](https://github.com/myorg/lke-landing-zone/blob/main/docs/adopter-guide.md#5-org-literals)",
			wantN:    1,
		},
		{
			name:    "absolute, anchor and mailto links are untouched",
			content: "[a](https://x.test/y) [b](#head) [c](mailto:x@y.test)",
			want:    "[a](https://x.test/y) [b](#head) [c](mailto:x@y.test)",
			wantN:   0,
		},
		{
			// Escaping the instance root is not ours to rewrite — we cannot know
			// what is out there.
			name:     "a link escaping the root is left alone",
			content:  "[up](../../elsewhere.md)",
			fileDir:  "",
			template: []string{"elsewhere.md"},
			want:     "[up](../../elsewhere.md)",
			wantN:    0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inInst, inTmpl := lookups(tc.instance, tc.template, tc.dirs)
			got, n := rewriteInstanceRootLinks(tc.content, tc.fileDir, inInst, inTmpl, "myorg")
			if got != tc.want {
				t.Errorf("rewrite:\n got %q\nwant %q", got, tc.want)
			}
			if n != tc.wantN {
				t.Errorf("count = %d, want %d", n, tc.wantN)
			}
		})
	}
}

// End to end over a real tree: deliver-docs must repoint the root-level link and
// leave docs/ to the existing docs-scoped pass.
func TestDeliverDocs_RepointsInstanceRoot(t *testing.T) {
	root := t.TempDir()
	tmpl := t.TempDir()

	mkdir := func(base string, parts ...string) string {
		p := filepath.Join(append([]string{base}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The template has the full docs set plus a template-only tree.
	write(filepath.Join(tmpl, "docs", "adopter-guide.md"), "# guide")
	write(filepath.Join(tmpl, "docs", "quickstart.md"), "# qs")
	mkdir(tmpl, "platform-apl")

	// The instance: docs/ already copied in, plus root-level Markdown.
	write(filepath.Join(root, "docs", "adopter-guide.md"), "# guide")
	write(filepath.Join(root, "docs", "quickstart.md"), "# qs")
	write(filepath.Join(root, "AGENTS.md"),
		"path: [docs/quickstart.md](docs/quickstart.md) and [docs/adopter-guide.md](docs/adopter-guide.md)\n"+
			"later: [`landingzone.yaml`](landingzone.yaml)\n")
	write(filepath.Join(root, "apl-values", "README.md"),
		"shared tree: [`platform-apl/`](../platform-apl/)\n")

	if err := runDeliverDocs(filepath.Join(root, "docs"), "myorg", "v1.2.3", root, tmpl); err != nil {
		t.Fatalf("runDeliverDocs: %v", err)
	}

	agents, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(agents)
	if !strings.Contains(got, "(docs/quickstart.md)") {
		t.Errorf("quickstart is still delivered — its link must stay relative:\n%s", got)
	}
	if !strings.Contains(got, "https://github.com/myorg/lke-landing-zone/blob/main/docs/adopter-guide.md") {
		t.Errorf("pruned adopter-guide link was not repointed:\n%s", got)
	}
	if !strings.Contains(got, "](landingzone.yaml)") {
		t.Errorf("landingzone.yaml is absent from BOTH trees and must be left alone:\n%s", got)
	}

	nested, err := os.ReadFile(filepath.Join(root, "apl-values", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nested), "/tree/main/platform-apl") {
		t.Errorf("template-only directory was not repointed to a tree URL:\n%s", nested)
	}
}

// Without --template-root the root pass must not run at all, so release-e2e's
// docs hoist (which has no template checkout to point at) is unchanged.
func TestDeliverDocs_RootPassIsOptIn(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "quickstart.md"), []byte("# qs"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := "[gone](docs/adopter-guide.md)\n"
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runDeliverDocs(filepath.Join(root, "docs"), "myorg", "v1.2.3", root, ""); err != nil {
		t.Fatalf("runDeliverDocs: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Errorf("root Markdown was rewritten without --template-root:\n got %q\nwant %q", after, before)
	}
}
