package main

// TestLinkResolution_AllThreeResolversAgreeOnRootRelative STAYED in package main,
// and it is the reason deliverdocs.RewriteInstanceRootLinks is exported at all.
//
// It pins an agreement BETWEEN THREE PACKAGES: docs-guard's two link resolvers
// (internal/docsguard) and the instance-root rewriter (internal/deliverdocs) must
// resolve a root-relative link to the same path, or the guard passes a link the
// rewriter then breaks. Only package main can see all three — it owns the root
// command the guard needs — so the test has to live here and one symbol has to
// cross the boundary for it. That is the whole of the export's justification.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/deliverdocs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/docsguard"
)

// THREE resolvers interpret a Markdown link path in this codebase — the guard's
// checkDocLinks and checkDeliveredDocLinks, and this rewriter. I fixed
// root-relative resolution in the first two and missed the third; review caught
// it. Rather than pin the rewriter alone, assert that all three AGREE, so the
// next divergence fails here instead of in review.
func TestLinkResolution_AllThreeResolversAgreeOnRootRelative(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("docs/quickstart.md", "# qs")

	// 1 + 2: the guard's resolvers must accept a root-relative link from a NESTED
	// file (joining to the file's dir would give docs/runbooks/docs/… and report it).
	mk("docs/runbooks/r.md", "[q](/docs/quickstart.md)\n")
	rep, err := docsguard.Run(root, docsguard.Options{SkipCommands: true, SkipWorkflows: true}, newRootCmd())
	if err != nil {
		t.Fatalf("docsguard.Run: %v", err)
	}
	if len(rep.Findings) != 0 {
		t.Errorf("the guard's link resolvers reported a valid root-relative link: %v", rep.Findings)
	}

	// 3: the rewriter must resolve it to the SAME place — proven by what it probes.
	var probed []string
	deliverdocs.RewriteInstanceRootLinks("[q](/docs/quickstart.md)", "apl-values",
		func(p string) bool { probed = append(probed, p); return true },
		func(string) (bool, bool) { return false, false }, "acme")
	if len(probed) != 1 || probed[0] != filepath.Join("docs", "quickstart.md") {
		t.Errorf("deliverdocs.RewriteInstanceRootLinks probed %v, want [docs/quickstart.md] — it must resolve root-relative links from the INSTANCE root like the other two", probed)
	}
}

// Nothing absolute or above the root may reach the existence probes, whatever a
// future caller passes as fileDir.
