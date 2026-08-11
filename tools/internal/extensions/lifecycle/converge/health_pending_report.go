package converge

// health_pending_report.go — what the convergence budget was waiting for when it
// ran out.
//
// Several health checks deliberately PEND rather than fail on states that a young
// cluster produces and an old one does not: a pod still being created, a Service
// whose backing pods have no PodIP yet. That choice is only defensible if the
// timeout report names them. A gate that swaps a precise "Service X has no
// endpoints" for "budget exhausted" has not become kinder — it has become less
// useful, and the operator now has to reconstruct by hand what the poll already
// knew.

import (
	"fmt"
	"os"
	"sort"
)

// convergePendingReportLimit bounds the list. A wedged cluster can have hundreds
// of not-OK resources, and a wall of them buries the few that matter as
// thoroughly as printing none.
const convergePendingReportLimit = 25

// reportConvergePending prints the resources the last poll was still waiting on.
func reportConvergePending(nonOK []string) {
	if len(nonOK) == 0 {
		fmt.Fprintln(os.Stderr, "::warning::the budget ran out while the last poll reported nothing "+
			"outstanding — the verdict and the census disagree, which is itself worth investigating.")
		return
	}
	items := append([]string(nil), nonOK...)
	sort.Strings(items)
	shown := items
	if len(shown) > convergePendingReportLimit {
		shown = shown[:convergePendingReportLimit]
	}
	fmt.Fprintf(os.Stderr, "::group::convergence — still waiting on %d item(s) when the budget ran out\n", len(items))
	for _, it := range shown {
		fmt.Fprintf(os.Stderr, "  • %s\n", it)
	}
	if len(items) > len(shown) {
		fmt.Fprintf(os.Stderr, "  … and %d more (showing the first %d)\n", len(items)-len(shown), len(shown))
	}
	fmt.Fprintln(os.Stderr, "::endgroup::")
	// The single most useful line goes OUTSIDE the group: a collapsed group is
	// exactly as invisible as no output when someone is skimming a failed run.
	fmt.Fprintf(os.Stderr, "::error::still not converged after the full budget — first outstanding item: %s\n", shown[0])
}
