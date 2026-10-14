package newinstance

// next_steps_test.go — the post-scaffold list is the quickstart an adopter reads
// with a terminal already open, so it outranks docs/quickstart.md in practice and
// has drifted from it in both directions before. No gate reads both; this is it.

import (
	"strings"
	"testing"
)

// TestReportCIImageSkew pins the WARNING, which is the entire deliverable of the
// `llz upgrade` half: the command cannot fix these (they are GitHub repo
// variables and it pushes nothing), so what it prints is all the operator gets
// before CI tells them the same thing 20 minutes later.
// TestPrintNextSteps guards the post-scaffold list, which is the quickstart an
// adopter actually reads — it is on their screen when the doc is not. It had
// drifted from docs/quickstart.md in both directions the drift can go: a step
// the doc gained (#405's `git push`) and a step the doc dropped (the deprecated
// `llz validate --env`). Neither gate that watches the other copy can see this
// one — docs-guard reads Markdown, and these are Go string literals — so the
// assertions live here.
func TestPrintNextSteps(t *testing.T) {
	out := captureStdout(t, func() { printNextSteps("my-instance", true) })

	// `llz env add` COMMITS and does not push, and the build renders from the
	// pushed tree — so omitting this walked every adopter into the exact state
	// #405 identified as one of two default-path slips.
	if !strings.Contains(out, "git push") {
		t.Errorf("next steps never tell the adopter to push:\n%s", out)
	}
	// Ordering is the whole point of a numbered path: pushing after the build was
	// dispatched is the same failure as not pushing at all.
	add, push := strings.Index(out, "llz env add"), strings.Index(out, "git push")
	build := strings.Index(out, "llz up")
	if !(add >= 0 && add < push && push < build) {
		t.Errorf("expected `llz env add` → `git push` → `llz up`; got indices %d, %d, %d:\n%s", add, push, build, out)
	}
	// Deprecated: prints a notice and points at `llz doctor --env`. Sending a
	// first-time adopter to it is how the CLI teaches its own retired surface.
	if strings.Contains(out, "llz validate --env") {
		t.Errorf("next steps still recommend the deprecated `llz validate --env`:\n%s", out)
	}
	// `llz up` chains tokens → doctor → build and stops at the first failure, but
	// it is interactive — an adopter who does not know that runs it unattended.
	if !strings.Contains(out, "interactive") {
		t.Errorf("next steps recommend `llz up` without saying it is interactive:\n%s", out)
	}
	// The individual gates have to stay reachable from here; they are what an
	// operator drops to when `llz up` stops on one of them.
	for _, want := range []string{"llz tokens --env", "llz doctor --env", "llz build <env>"} {
		if !strings.Contains(out, want) {
			t.Errorf("next steps no longer mention %q:\n%s", want, out)
		}
	}
}
