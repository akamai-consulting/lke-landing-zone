package converge

// cobra_incluster.go — the CLI surface for incluster.
//
// Split from incluster.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"os"

	"github.com/spf13/cobra"
)

func HealthInClusterCmd() *cobra.Command {
	// failOnUnhealthy defaults true (exit per the convergence contract). =false is
	// report-only (always exit 0) — how a scheduled/report run drives it without a
	// shell, since the distroless image can't do `… || true`.
	failOnUnhealthy := true
	c := &cobra.Command{
		Use:   "health-incluster",
		Short: "kubectl-free cluster convergence check for in-cluster runners (exit 0 converged / 2 in-progress / 1 hard-failed / 3 unreachable)",
		Long: "The internal/kube (no kubectl) sibling of `llz ci health`, for a day-2 Argo\n" +
			"Workflow on the slim distroless llz image. Classifies Argo CD Application\n" +
			"convergence via the pod ServiceAccount and exits per the convergence\n" +
			"contract. --fail-on-unhealthy=false → report-only: exit 0 on an unhealthy\n" +
			"cluster VERDICT (1/2), but still exit 3 if the apiserver is unreachable —\n" +
			"the check couldn't run, which is worth failing the job on.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// DELIBERATE os.Exit, not a returned error: this verb's exit codes are
			// a contract, and more than 0/1 are load-bearing. 2 (in-progress) is
			// distinct from 1 (hard-failed) — an Argo Workflow retry policy treats
			// them differently — and 3 (apiserver unreachable) means "the check
			// could not RUN", which --fail-on-unhealthy=false deliberately does NOT
			// suppress while it does suppress 1 and 2. Returning an error would
			// collapse all three into cobra's exit 1 and erase those distinctions.
			os.Exit(healthInClusterExitCode(cmd.Context(), failOnUnhealthy))
			return nil
		},
	}
	c.Flags().BoolVar(&failOnUnhealthy, "fail-on-unhealthy", true,
		"exit non-zero per the convergence contract on an unhealthy cluster; =false is report-only (exit 0 on a 1/2 verdict; still exits 3 if the apiserver is unreachable)")
	return c
}
