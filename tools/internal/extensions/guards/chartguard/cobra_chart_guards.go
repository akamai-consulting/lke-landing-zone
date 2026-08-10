package chartguard

// cobra_chart_guards.go — the three chart gates, reduced to flag sets.
//
// Everything they do is `guard-charts` and lives in tools/internal/extensions/guards/chartguard.
// What stays here is one seam: git.
//
// THE SMALLEST Deps IN THE REPO, and that is the shape of a gate rather than an
// accident. A gate reaches nothing — no cluster, no cloud, no credential — so the
// only capability it cannot supply itself is asking git what CHANGED, which is
// what chart-version-guard is about: publish-charts only ever pushes a NEW
// version, so an edit inside a chart directory that does not move Chart.yaml is
// a chart that will 404 at Argo sync time.

import (
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/gitcmd"
	"github.com/spf13/cobra"
)

func ChartguardDeps() Deps {
	return Deps{GitOutput: gitcmd.Output}
}

func ChartLockDriftCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "chart-lock-drift [chart-dir...]",
		Short: "fail when a chart's committed Chart.lock drifts from its Chart.yaml dependencies",
		Long: "Native port of check-chart-lock-drift.py (the Makefile's helm-dep-lock-check).\n" +
			"For each chart directory, compares Chart.lock against the dependency\n" +
			"declarations in Chart.yaml and fails if any dependency's name, version, or\n" +
			"repository differs (or Chart.lock is missing) — meaning Chart.yaml was updated\n" +
			"without re-running `helm dependency update`.",
		// NO ARGS MEANS EVERY FIRST-PARTY CHART, and that is what made this gate
		// driveable at all.
		//
		// It was one of two gates registry/gates.go could not run, both filed under
		// "the caller chooses the subject". Reading them side by side, they are not
		// the same shape: `check-coverage` takes a COVERPROFILE — a build artifact
		// that does not exist until `go test` has run, plus a floor list the Makefile
		// deliberately keeps overridable. This one takes chart directories, which are
		// simply `kubernetes-charts/*/` and were already enumerated ten lines away by
		// loadLocalChartVersions for chart-pin-guard.
		//
		// So this was a missing DEFAULT, not a gap in the model. It is worth saying
		// plainly because the two cases together looked like the two-case bar for a
		// new vocabulary word ("let a binding declare its own corpus"), and one of
		// them dissolving means that bar is NOT met — `check-coverage` stands alone,
		// and one case is an anecdote. See undrivenGates.
		//
		// The positionals still work, and the Makefile still passes $(OPENBAO_CHART):
		// a caller narrowing the subject is the reason the flag set exists.
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				found, err := firstPartyChartDirs(root)
				if err != nil {
					return err
				}
				args = found
			}
			return RunLockDrift(root, args, os.Stdout)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root the chart dirs are relative to")
	return c
}

func ChartPinGuardCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "chart-pin-guard",
		Short: "fail when an Argo chart pin drifts from the local Chart.yaml version",
		Long: "Scans the live apl-values Argo Application manifests and the\n" +
			"llz-argo-bootstrap-apps component list for first-party llz-* chart pins\n" +
			"(targetRevision / version) and fails if any pin disagrees with that chart's\n" +
			"kubernetes-charts/<chart>/Chart.yaml version. A pin ahead of (or behind) the\n" +
			"published chart 404s at Argo sync time — on a cold bootstrap that silently\n" +
			"strands the support-plane app and times out the OpenBao bootstrap.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunPinGuard(root)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root")
	return c
}

func ChartVersionGuardCmd() *cobra.Command {
	var base, root string
	c := &cobra.Command{
		Use:   "chart-version-guard",
		Short: "fail when a chart changes without bumping its Chart.yaml version",
		Long: "Diffs each kubernetes-charts/<chart>/ directory this PR touches against the\n" +
			"PR base and fails if that chart's Chart.yaml version: is unchanged. publish-\n" +
			"charts.yml publishes immutably (only a new version is pushed), so a chart change\n" +
			"merged without a version bump is never published and clusters keep the stale\n" +
			"chart. New charts (no Chart.yaml at the base) and removed charts are exempt.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunVersionGuard(ChartguardDeps(), base, root)
		},
	}
	c.Flags().StringVar(&base, "base", "", "git ref/SHA of the PR base to diff against (required)")
	c.Flags().StringVar(&root, "root", ".", "repository root")
	return c
}
