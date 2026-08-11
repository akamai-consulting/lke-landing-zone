package converge

// health_pending_report_test.go — deferring a verdict to the budget is only
// honest if the budget's report says what it was waiting for.

import (
	"strings"
	"testing"
)

func TestReportConvergePendingNamesTheStragglers(t *testing.T) {
	out := captureStderr(t, func() {
		reportConvergePending([]string{
			"Service llz-observability/otel-collector has no endpoints yet",
			"Pod otel/platform-logs-collector-x still starting",
		})
	})
	for _, want := range []string{"otel-collector", "platform-logs-collector-x", "still waiting on 2 item(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("the timeout report does not name %q — the operator has to reconstruct by hand what "+
				"the poll already knew:\n%s", want, out)
		}
	}
	// The headline must be OUTSIDE the collapsed group: a ::group:: is exactly as
	// invisible as no output to someone skimming a failed run.
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "::error::") && strings.Contains(ln, "first outstanding item") {
			return
		}
	}
	t.Errorf("no ungrouped ::error:: naming the first outstanding item:\n%s", out)
}

// A long list must be bounded — a wall of items buries the few that matter as
// thoroughly as printing none.
func TestReportConvergePendingBoundsTheList(t *testing.T) {
	many := make([]string, 100)
	for i := range many {
		many[i] = "item-" + string(rune('a'+i%26)) + "-x"
	}
	out := captureStderr(t, func() { reportConvergePending(many) })
	if !strings.Contains(out, "and 75 more") {
		t.Errorf("the list was not bounded, or the remainder was not reported:\n%s", out)
	}
}

// The budget running out while the poll reports nothing outstanding is a
// contradiction, and saying so is more useful than printing an empty list.
func TestReportConvergePendingFlagsAnEmptyCensus(t *testing.T) {
	out := captureStderr(t, func() { reportConvergePending(nil) })
	if !strings.Contains(out, "disagree") {
		t.Errorf("an empty census should be called out as a contradiction:\n%s", out)
	}
}
