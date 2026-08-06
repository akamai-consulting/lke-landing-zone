package main

// ci_chart_guards.go — the three chart gates, reduced to flag sets.
//
// Everything they do is `guard-charts` and lives in tools/internal/chartguard.
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

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/chartguard"
)

func chartguardDeps() chartguard.Deps {
	return chartguard.Deps{GitOutput: gitOutput}
}

func ciChartLockDriftCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "chart-lock-drift <chart-dir>...",
		Short: "fail when a chart's committed Chart.lock drifts from its Chart.yaml dependencies",
		Long: "Native port of check-chart-lock-drift.py (the Makefile's helm-dep-lock-check).\n" +
			"For each chart directory, compares Chart.lock against the dependency\n" +
			"declarations in Chart.yaml and fails if any dependency's name, version, or\n" +
			"repository differs (or Chart.lock is missing) — meaning Chart.yaml was updated\n" +
			"without re-running `helm dependency update`.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return chartguard.RunLockDrift(root, args, os.Stdout)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root the chart dirs are relative to")
	return c
}

func ciChartPinGuardCmd() *cobra.Command {
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
			return chartguard.RunPinGuard(root)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root")
	return c
}

func ciChartVersionGuardCmd() *cobra.Command {
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
			return chartguard.RunVersionGuard(chartguardDeps(), base, root)
		},
	}
	c.Flags().StringVar(&base, "base", "", "git ref/SHA of the PR base to diff against (required)")
	c.Flags().StringVar(&root, "root", ".", "repository root")
	return c
}
