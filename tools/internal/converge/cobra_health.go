package converge

// cobra_health.go — the CLI surface for health.
//
// Split from health.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func HealthCmd() *cobra.Command {
	// failOnUnhealthy defaults true so a bare `llz ci health` keeps its
	// convergence-contract exit semantics (existing callers unchanged). Passing
	// --fail-on-unhealthy=false is REPORT-ONLY: it still runs every check and
	// prints the report, but always exits 0. That lets a shell-less caller (the
	// distroless llz image — no /bin/sh, so no `… || true`) choose report vs gate
	// with a plain value flag instead of a shell conditional. See the
	// clusterHealthWorkflow component's WorkflowTemplate.
	failOnUnhealthy := true
	c := &cobra.Command{
		Use:   "health",
		Short: "cluster convergence health check (exit 0 converged / 2 in-progress / 1 hard-failed / 3 unreachable)",
		Long: "Native port of check-cluster-health.sh — the single source of truth for \"is\n" +
			"the cluster converged?\". Runs every in-cluster check (foundations, OpenBao,\n" +
			"cert-manager, ESO, Argo apps, workloads, storage, jobs, …) against the cluster\n" +
			"$KUBECONFIG points at, classifying each via the unit-tested internal/health\n" +
			"predicates, and exits per the convergence contract: 1 hard-failed, 2 in-\n" +
			"progress (poll), 0 converged, 3 apiserver unreachable (an infrastructure\n" +
			"transient, retried against the budget — never a hard strike).\n\n" +
			"--fail-on-unhealthy=false → report-only: run the checks + print the report but\n" +
			"always exit 0 (for a report-only scheduled run on a shell-less image).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			code := healthExitCode()
			if !failOnUnhealthy {
				if code != 0 {
					fmt.Fprintf(os.Stderr, "::notice::health exit %d suppressed (--fail-on-unhealthy=false, report-only)\n", code)
				}
				return nil // exit 0
			}
			// DELIBERATE os.Exit, not a returned error: every code in the
			// convergence contract is load-bearing and callers DISTINGUISH them.
			// `llz ci converge` (runConverge → health.ConvergeStep) branches on
			// 0 converged / 1 hard-failed / 2 in-progress (keep polling) /
			// 3 apiserver unreachable (retry without spending a hard strike).
			// Returning an error would collapse 2 and 3 into cobra's exit 1 and
			// turn every transient into an immediate hard failure.
			os.Exit(code)
			return nil
		},
	}
	c.Flags().BoolVar(&failOnUnhealthy, "fail-on-unhealthy", true,
		"exit non-zero per the convergence contract on an unhealthy cluster; =false is report-only (always exit 0)")
	return c
}
func ConvergeCmd() *cobra.Command {
	var budget, interval, retryDelay int
	c := &cobra.Command{
		Use:   "converge",
		Short: "poll `llz ci health` until the cluster converges or the budget runs out",
		Long: "Native port of converge.sh. Polls `llz ci health` (exit 0/1/2/3): converged\n" +
			"-> exit 0; in-progress -> sleep --interval and re-run until --budget elapses\n" +
			"(then exit 1); hard-failed -> re-run once after --retry-delay to absorb a\n" +
			"transient, and exit 1 only if it hard-fails twice in a row; apiserver\n" +
			"unreachable -> re-run after --retry-delay against the budget without spending\n" +
			"a hard strike (a blip can't trip the twice-in-a-row abort).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runConverge(budget, interval, retryDelay)
		},
	}
	c.Flags().IntVar(&budget, "budget", 1800, "total elapsed-time budget in seconds")
	c.Flags().IntVar(&interval, "interval", 30, "seconds between in-progress polls")
	c.Flags().IntVar(&retryDelay, "retry-delay", 60, "seconds before re-running a hard-fail check")
	return c
}
