package main

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/onboard"
)

// printNextSteps is commands.go's; its test travelled with image_repin_test.go
// and came back. THIRD file this iteration holding tests with different subjects.

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
func TestRepinPlanNote(t *testing.T) {
	if got := onboard.RepinPlanNote(nil); got != "" {
		t.Errorf("onboard.RepinPlanNote(nil) = %q; want empty", got)
	}
	// A dry run that reports "0 missing REQUIRED item(s)" and stops reads as "no
	// work" to the one operator who has some.
	got := onboard.RepinPlanNote([]templatecommit.ImageSkew{{Name: "TF_IMAGE"}, {Name: "KUBE_IMAGE"}})
	if !strings.Contains(got, "2") || !strings.Contains(got, "re-pin") {
		t.Errorf("onboard.RepinPlanNote = %q; want it to count the re-pins", got)
	}
}
