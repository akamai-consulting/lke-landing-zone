package selfupgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedInstance writes files into a throwaway instance root and returns it.
func seedInstance(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// THE INCIDENT, reproduced. A real adopter linked their own managed-apps playbook
// from six `managed` files. One upgrade rewrote all six from a clean render — the
// template cannot know about a file it does not ship — and the playbook was left
// present but unreachable. Nothing was broken, so nothing reported it.
func TestDroppedRefsReportsALinkWhoseTargetIsStillInTheInstance(t *testing.T) {
	root := seedInstance(t, map[string]string{
		"docs/playbooks/managed-apps-onboarding.md": "# local playbook\n",
	})
	before := []byte("See [managed apps](playbooks/managed-apps-onboarding.md) for onboarding.\n")
	after := []byte("No local links here any more.\n")

	got := droppedRefs("docs/README.md", before, after, root)
	if len(got) != 1 {
		t.Fatalf("want 1 dropped ref, got %d: %+v", len(got), got)
	}
	if got[0].Target != "playbooks/managed-apps-onboarding.md" {
		t.Errorf("target = %q", got[0].Target)
	}
}

// THE NOISE FLOOR, and it is the reason the check is worth having. A release drops
// links constantly — docs move, sections get rewritten. Those targets are gone from
// the new render too, so reporting them would fire on every upgrade and the report
// would be ignored, which is worse than not having it.
func TestDroppedRefsIgnoresALinkWhoseTargetIsAlsoGone(t *testing.T) {
	root := seedInstance(t, map[string]string{"docs/README.md": "x"})
	before := []byte("See [an old doc](retired-page.md).\n")
	after := []byte("Rewritten.\n")

	if got := droppedRefs("docs/README.md", before, after, root); len(got) != 0 {
		t.Errorf("reported a link whose target no longer exists — every release would trip this: %+v", got)
	}
}

func TestDroppedRefsIgnoresLinksThatSurvived(t *testing.T) {
	root := seedInstance(t, map[string]string{"docs/playbooks/local.md": "x"})
	before := []byte("[local](playbooks/local.md)\n")
	after := []byte("Reworded, same link: [local](playbooks/local.md)\n")

	if got := droppedRefs("docs/README.md", before, after, root); len(got) != 0 {
		t.Errorf("reported a link that is still present: %+v", got)
	}
}

// Off-instance targets are the template's business. A dropped https:// link is not
// evidence the adopter lost anything.
func TestDroppedRefsIgnoresNonInstanceTargets(t *testing.T) {
	root := seedInstance(t, map[string]string{"docs/README.md": "x"})
	before := []byte("[up](https://github.com/x/y) [mail](mailto:a@b) [anchor](#section)\n")
	after := []byte("gone\n")

	if got := droppedRefs("docs/README.md", before, after, root); len(got) != 0 {
		t.Errorf("reported a non-instance target: %+v", got)
	}
}

// Relative targets resolve from the FILE's directory — what Markdown means, and
// what a reader clicking the link would get. Resolving from the instance root
// instead would miss every link in a nested doc, which is most of them.
func TestDroppedRefsResolvesRelativeToTheFilesDirectory(t *testing.T) {
	root := seedInstance(t, map[string]string{"docs/playbooks/team.md": "x"})
	before := []byte("[team](team.md)\n")
	after := []byte("gone\n")

	got := droppedRefs("docs/playbooks/argocd-ops.md", before, after, root)
	if len(got) != 1 {
		t.Fatalf("a sibling link did not resolve from the file's own directory: %+v", got)
	}
}

func TestDroppedRefsStripsAnchorsBeforeResolving(t *testing.T) {
	root := seedInstance(t, map[string]string{"docs/quickstart.md": "x"})
	before := []byte("[step 3](quickstart.md#step-3)\n")
	after := []byte("gone\n")

	if got := droppedRefs("docs/README.md", before, after, root); len(got) != 1 {
		t.Errorf("an anchored link to an existing file was not reported: %+v", got)
	}
}

// Non-Markdown managed files are rewritten wholesale every release; scanning them
// for "links" would be noise with no reader.
func TestDroppedRefsOnlyConsidersMarkdown(t *testing.T) {
	root := seedInstance(t, map[string]string{"a.yaml": "x"})
	if got := droppedRefs("workflow.yaml", []byte("[a](a.yaml)"), []byte(""), root); len(got) != 0 {
		t.Errorf("scanned a non-Markdown file: %+v", got)
	}
}

func TestFormatDroppedRefsNamesTheDurableFix(t *testing.T) {
	if FormatDroppedRefs(nil) != "" {
		t.Error("empty input must produce no advisory at all")
	}
	s := FormatDroppedRefs([]DroppedRef{
		{File: "docs/README.md", Target: "playbooks/managed-apps-onboarding.md"},
		{File: "AGENTS.md", Target: "kubernetes-custom/"},
	})
	for _, want := range []string{"docs/README.md", "AGENTS.md", "managed-apps-onboarding.md", "docs/local.md", "owned"} {
		if !strings.Contains(s, want) {
			t.Errorf("advisory does not mention %q:\n%s", want, s)
		}
	}
}
