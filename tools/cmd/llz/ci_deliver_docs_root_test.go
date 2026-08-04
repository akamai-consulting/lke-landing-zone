package main

// Tests for the instance-ROOT link repoint — the half repointReferencedLinks
// never covered, which is why AGENTS.md → docs/adopter-guide.md shipped dead in
// every rendered instance.

import (
	"fmt"
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

	// The template has the full docs set plus a template-only tree...
	write(filepath.Join(tmpl, "docs", "adopter-guide.md"), "# guide")
	write(filepath.Join(tmpl, "docs", "quickstart.md"), "# qs")
	mkdir(tmpl, "platform-apl")
	// ...and, under instance-template/, the scaffold files it renders into an
	// instance. Only these are template-OWNED, so only these may be rewritten.
	write(filepath.Join(tmpl, "instance-template", "AGENTS.md"), "scaffold source")
	write(filepath.Join(tmpl, "instance-template", "apl-values", "README.md"), "scaffold source")

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

// deliver-docs runs on `copier update` against a LIVE instance, which holds
// Markdown that is none of our business — a vendored chart, a .terraform module
// cache, an adopter's own notes. An early cut of the root pass walked all of it
// and rewrote a link inside vendor/, mutating a file the template does not own.
func TestDeliverDocs_LeavesNonTemplateFilesAlone(t *testing.T) {
	root, tmpl := t.TempDir(), t.TempDir()
	write := func(base string, parts ...string) func(string) {
		return func(body string) {
			p := filepath.Join(append([]string{base}, parts...)...)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Template: the referenced doc, plus the ONE scaffold file it owns.
	write(tmpl, "docs", "adopter-guide.md")("# guide")
	write(tmpl, "instance-template", "AGENTS.md")("scaffold source")

	// Instance: a delivered docs/, the owned AGENTS.md, and three files that are
	// NOT ours — each linking the same pruned doc, so only ownership separates them.
	write(root, "docs", "quickstart.md")("# qs")
	link := "see [g](%s)\n"
	write(root, "AGENTS.md")(fmt.Sprintf(link, "docs/adopter-guide.md"))
	write(root, "vendor", "chart", "README.md")(fmt.Sprintf(link, "../../docs/adopter-guide.md"))
	write(root, ".terraform", "modules", "m", "README.md")(fmt.Sprintf(link, "../../../docs/adopter-guide.md"))
	write(root, "my-team-notes", "onboarding.md")(fmt.Sprintf(link, "../docs/adopter-guide.md"))

	if err := runDeliverDocs(filepath.Join(root, "docs"), "acme", "v1", root, tmpl); err != nil {
		t.Fatalf("runDeliverDocs: %v", err)
	}

	owned, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(owned), "https://github.com/acme/") {
		t.Errorf("the template-owned AGENTS.md should have been repointed:\n%s", owned)
	}
	for _, rel := range []string{
		filepath.Join("vendor", "chart", "README.md"),
		filepath.Join(".terraform", "modules", "m", "README.md"),
		filepath.Join("my-team-notes", "onboarding.md"),
	} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "https://github.com/") {
			t.Errorf("%s is NOT template-owned and must not be rewritten:\n%s", rel, b)
		}
	}
}

// --docs and --root are INDEPENDENT flags, and the two callers spell them
// differently: copier passes `--docs docs --root .` (relative, CWD = instance),
// e2e passes `--docs .e2e-instance/docs --root .e2e-instance`. An early cut
// compared cleaned path STRINGS, so a caller mixing an absolute --root with a
// relative --docs failed to recognise docs/ and walked it a second time — under
// the root pass, which is not the pass that owns it. Identity is by inode now;
// this pins that across all four spellings.
func TestDeliverDocs_DocsDirRecognisedRegardlessOfSpelling(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, root, docs string }{
		{"both relative", ".", "docs"},
		{"both absolute", "", ""}, // filled below
		{"absolute root, relative docs", "", "docs"},
		{"relative root, absolute docs", ".", ""},
		// The case string comparison actually gets wrong: the same directory
		// reached by two different names. filepath.Abs does not resolve symlinks,
		// so "<base>/docs" and "<base>/docs-link" compare unequal while being one
		// inode — and the root pass then walks docs/ as if it were not docs/.
		{"docs reached through a symlink", ".", "SYMLINK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, tmpl := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Join(base, "docs"), 0o755); err != nil {
				t.Fatal(err)
			}
			// The probe link is a DIRECTORY, deliberately: the docs pass only
			// rewrites *.md, so it leaves this alone — while the root pass would
			// resolve docs/../platform-apl and repoint it. A /tree/ URL appearing
			// here is therefore proof, and the only proof, that the root pass
			// walked docs/. (A *.md probe cannot discriminate: both passes would
			// emit the same blob/main/docs/... string.)
			if err := os.WriteFile(filepath.Join(base, "docs", "quickstart.md"),
				[]byte("[shared tree](../platform-apl/)\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// Make it template-OWNED under docs/, so only the docs-skip can save it.
			if err := os.MkdirAll(filepath.Join(tmpl, templateScaffoldSubdir, "docs"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tmpl, templateScaffoldSubdir, "docs", "quickstart.md"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(tmpl, "platform-apl"), 0o755); err != nil {
				t.Fatal(err)
			}
			// Chdir is process-global, and `base` is removed when THIS subtest
			// ends — so the restore must be scoped here, not to the parent.
			// Leaving it to the parent left the process sitting in a deleted
			// directory between subtests, which broke an unrelated timing-
			// sensitive test elsewhere in the package.
			if err := os.Chdir(base); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(wd) })
			root, docs := tc.root, tc.docs
			if root == "" {
				root = base
			}
			if docs == "" {
				docs = filepath.Join(base, "docs")
			}
			if docs == "SYMLINK" {
				link := filepath.Join(base, "docs-link")
				if err := os.Symlink(filepath.Join(base, "docs"), link); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				docs = link
			}
			if err := runDeliverDocs(docs, "acme", "v1", root, tmpl); err != nil {
				t.Fatalf("runDeliverDocs: %v", err)
			}
			b, err := os.ReadFile(filepath.Join(base, "docs", "quickstart.md"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "/tree/main/platform-apl") {
				t.Errorf("root pass walked docs/ (spelling: root=%q docs=%q):\n%s", tc.root, tc.docs, b)
			}
		})
	}
}

// Both walks in deliver-docs used to return nil on a WalkDir error, so an
// unreadable subtree was skipped and the run still reported success — a stale
// link surviving a delivery that claimed to have fixed it. Same false-green class
// as docs-guard's. Both must now fail closed.
func TestDeliverDocs_WalkErrorFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — an unreadable dir is still traversable")
	}
	for _, tc := range []struct{ name, blockedRel string }{
		{"the instance-root pass", "sub"}, // repointInstanceRootLinks
		// Must live under a KEPT dir: deliver-docs prunes docs/ first, and a
		// blocked dir outside the keep-set fails at the PRUNE instead — which
		// made the first cut of this subtest pass without ever reaching the walk.
		{"the docs-scoped pass", "docs/runbooks/nested"}, // repointReferencedLinks
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, tmpl := t.TempDir(), t.TempDir()
			mk := func(base, rel, body string) {
				p := filepath.Join(base, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			mk(root, "docs/quickstart.md", "# qs")
			mk(tmpl, "instance-template/AGENTS.md", "src")
			mk(root, "AGENTS.md", "[g](docs/gone.md)\n")

			blocked := filepath.Join(root, tc.blockedRel)
			if err := os.MkdirAll(blocked, 0o755); err != nil {
				t.Fatal(err)
			}
			mk(root, filepath.Join(tc.blockedRel, "x.md"), "# x")
			if err := os.Chmod(blocked, 0o000); err != nil {
				t.Skipf("cannot chmod: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

			err := runDeliverDocs(filepath.Join(root, "docs"), "acme", "v1", root, tmpl)
			if err == nil {
				t.Error("a walk error was swallowed — deliver-docs must fail closed, not report a delivery it did not complete")
			}
		})
	}
}

// deliver-docs rewrites template-owned Markdown IN PLACE on a live instance, so
// it must not disturb file modes. A review flagged the `0o644` argument to
// os.WriteFile as resetting perms; measured, it does not — that argument applies
// only when CREATING, and these paths are always read first, so the create branch
// is unreachable. This pins the behaviour rather than "fixing" a hazard that is
// not there: if anyone later switches to a create-truncate pattern, this fails.
func TestDeliverDocs_PreservesFileModes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — mode semantics differ")
	}
	root, tmpl := t.TempDir(), t.TempDir()
	write := func(base, rel, body string, mode os.FileMode) string {
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write(tmpl, "docs/adopter-guide.md", "# guide", 0o644)
	write(tmpl, filepath.Join(templateScaffoldSubdir, "AGENTS.md"), "src", 0o644)
	write(root, "docs/quickstart.md", "# qs", 0o644)
	// A deliberately restrictive mode on a file the ROOT pass will rewrite.
	agents := write(root, "AGENTS.md", "[g](docs/adopter-guide.md)\n", 0o600)
	// ...and on one the DOCS pass will rewrite.
	kept := write(root, "docs/runbooks/r.md", "[s](../secrets.md)\n", 0o640)

	if err := runDeliverDocs(filepath.Join(root, "docs"), "acme", "v1", root, tmpl); err != nil {
		t.Fatalf("runDeliverDocs: %v", err)
	}

	for _, tc := range []struct {
		path string
		want os.FileMode
	}{{agents, 0o600}, {kept, 0o640}} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != tc.want {
			t.Errorf("%s: mode %v, want %v preserved — an in-place doc rewrite must not change perms on a live instance",
				filepath.Base(tc.path), got, tc.want)
		}
		// And prove the rewrite actually happened, so this is not passing vacuously.
		b, _ := os.ReadFile(tc.path)
		if !strings.Contains(string(b), "https://github.com/acme/") {
			t.Errorf("%s was not rewritten — the mode assertion proves nothing:\n%s", filepath.Base(tc.path), b)
		}
	}
}
