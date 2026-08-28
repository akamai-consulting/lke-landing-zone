package converge

// cobra_health.go — the CLI surface for health.
//
// Split from health.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"fmt"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/exitcode"
	"os"

	"github.com/spf13/cobra"
)

// scopeCheck validates --scope BEFORE the command body, and fails with an exit
// code OUTSIDE the convergence contract.
//
// Both halves matter. Validating inside RunE meant a typo ran nothing and then
// exited 1 — which in these commands means "the cluster hard-failed", so a caller
// retried, failed identically, and reported operator intervention required for a
// cluster nothing had looked at. Returning a plain error from PreRunE fixes the
// ordering but NOT the code: cli.Main routes every non-Passthrough error to 1.
// exitcode 64 (EX_USAGE) is the one answer no health verdict can be confused with.
func scopeCheck(scope *string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		if *scope != ScopePlatform && *scope != ScopeApps {
			cmd.SilenceUsage = true
			// Passthrough carries a code and not a message, so the explanation is
			// printed here — as an annotation, since the caller of a red step needs to
			// see it without opening the log.
			fmt.Fprintf(os.Stderr, "::error::--scope must be %q or %q, got %q — nothing was checked (exit 64, a usage error, NOT a cluster verdict)\n",
				ScopePlatform, ScopeApps, *scope)
			return &exitcode.Passthrough{Code: 64}
		}
		return nil
	}
}

func HealthCmd() *cobra.Command {
	// failOnUnhealthy defaults true so a bare `llz ci health` keeps its
	// convergence-contract exit semantics (existing callers unchanged). Passing
	// --fail-on-unhealthy=false is REPORT-ONLY: it still runs every check and
	// prints the report, but always exits 0. That lets a shell-less caller (the
	// distroless llz image — no /bin/sh, so no `… || true`) choose report vs gate
	// with a plain value flag instead of a shell conditional. See the
	// clusterHealthWorkflow component's WorkflowTemplate.
	failOnUnhealthy := true
	scope := ScopePlatform
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
			"always exit 0 (for a report-only scheduled run on a shell-less image).\n\n" +
			"--scope selects WHICH HALF of the same report decides the exit code. platform\n" +
			"(default) is the convergence contract: instance-owned content — the operator\n" +
			"escape hatch and the apps deployed through it — is reported but never gates, so\n" +
			"a platform release is not blocked by an app team's missing credential. apps is\n" +
			"the other half: it gates on exactly that instance-owned content and ignores the\n" +
			"platform's, so the app lane keeps a gate of its own instead of borrowing the\n" +
			"platform's. Both run the same single scan; neither hides the other's findings\n" +
			"from the printed report.",
		Args: cobra.NoArgs,
		// The scope is validated BEFORE the command body, and its error is a usage
		// error rather than a health verdict. Validating inside RunE made a typo
		// (`--scope=app`) exit 1 — which in this command means "the cluster hard
		// failed" — so a caller would retry, fail identically, and report operator
		// intervention required for a cluster nothing ever looked at.
		PreRunE: scopeCheck(&scope),
		RunE: func(_ *cobra.Command, _ []string) error {
			code := healthExitCodeFor(scope)
			if code != 0 && scope == ScopeApps {
				// Name the scope in the annotation: the report prints BOTH verdicts,
				// and a reader who sees a red step above a green platform line has to
				// be told which half of it decided the exit.
				fmt.Fprintf(os.Stderr, "::error::apps scope: instance-owned content is not converged (exit %d). The platform contract does not gate on it — this step does.\n", code)
			}
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
	c.Flags().StringVar(&scope, "scope", ScopePlatform,
		"which half of the report decides the exit code: platform (the convergence contract) or apps (instance-owned content)")
	c.Flags().BoolVar(&failOnUnhealthy, "fail-on-unhealthy", true,
		"exit non-zero per the convergence contract on an unhealthy cluster; =false is report-only (always exit 0)")
	return c
}
func ConvergeCmd() *cobra.Command {
	var budget, interval, retryDelay int
	scope := ScopePlatform
	c := &cobra.Command{
		Use:   "converge",
		Short: "poll `llz ci health` until the cluster converges or the budget runs out",
		Long: "Native port of converge.sh. Polls `llz ci health` (exit 0/1/2/3): converged\n" +
			"-> exit 0; in-progress -> sleep --interval and re-run until --budget elapses\n" +
			"(then exit 1); hard-failed -> re-run once after --retry-delay to absorb a\n" +
			"transient, and exit 1 only if it hard-fails twice in a row; apiserver\n" +
			"unreachable -> re-run after --retry-delay against the budget without spending\n" +
			"a hard strike (a blip can't trip the twice-in-a-row abort).\n\n" +
			"--scope picks which half of the same report the loop polls on: platform\n" +
			"(default) or apps (instance-owned content). The app lane polls rather than\n" +
			"taking a one-shot reading because it needs the same tolerances the platform\n" +
			"lane has — a budget, and the in-budget classifiers that call a pod being\n" +
			"created 'starting' instead of 'failed'.",
		Args:    cobra.NoArgs,
		PreRunE: scopeCheck(&scope),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runConverge(budget, interval, retryDelay, scope)
		},
	}
	c.Flags().StringVar(&scope, "scope", ScopePlatform,
		"which half of the report the loop polls on: platform (the convergence contract) or apps (instance-owned content)")
	c.Flags().IntVar(&budget, "budget", 1800, "total elapsed-time budget in seconds")
	c.Flags().IntVar(&interval, "interval", 30, "seconds between in-progress polls")
	c.Flags().IntVar(&retryDelay, "retry-delay", 60, "seconds before re-running a hard-fail check")
	return c
}
