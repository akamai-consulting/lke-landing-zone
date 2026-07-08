package main

// ci_health_incluster.go — `llz ci health-incluster`: the KUBECTL-FREE sibling of
// `llz ci health`, for a day-2 job that runs INSIDE the cluster on the slim
// distroless llz image (no kubectl, no shell). It computes the cluster
// convergence verdict — the same 0/1/2/3 exit-code contract — over `internal/kube`
// (the hand-rolled REST client authenticated by the pod ServiceAccount) instead of
// shelling out to kubectl, reusing the reconciler's convergence classifier
// (reconcile_convergence.go → convergenceReport, the same health.ClassifyArgoApp
// predicate). This is what makes the clusterHealthWorkflow Argo WorkflowTemplate
// runnable in-cluster with no GitHub secrets (docs/designs/day2-incluster-health.md).
//
// Scope: Argo CD Application convergence — the canonical convergence signal the
// contract's readiness gate waits on. The reconciler's supplementary in-cluster
// gauges (ESO store, cert-manager, OpenBao seal — reconcile_health.go, also
// kubectl-free) can be folded in later; this is the exit-code core.
//
// `llz ci health` (kubectl) stays the source of truth for the CI/terraform
// converge gate; this is the in-cluster exit-code sibling — one predicate library,
// two callers.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kube"
	"github.com/spf13/cobra"
)

func ciHealthInClusterCmd() *cobra.Command {
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
			os.Exit(healthInClusterExitCode(cmd.Context(), failOnUnhealthy))
			return nil
		},
	}
	c.Flags().BoolVar(&failOnUnhealthy, "fail-on-unhealthy", true,
		"exit non-zero per the convergence contract on an unhealthy cluster; =false is report-only (exit 0 on a 1/2 verdict; still exits 3 if the apiserver is unreachable)")
	return c
}

// healthInClusterExitCode builds the in-cluster client, computes the convergence
// report, prints it, and returns the exit code. apiserver-unreachable → 3.
func healthInClusterExitCode(ctx context.Context, failOnUnhealthy bool) int {
	client, err := kube.NewInCluster()
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::cannot build in-cluster Kubernetes client (is this running in a pod with a ServiceAccount?): %v\n", err)
		return 3
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	r, crdPresent, err := convergenceReport(cctx, client)
	if err != nil {
		// A query failure here is apiserver-unreachable-class: exit 3 (transient),
		// not a cluster hard-fail — matching `llz ci health`'s exit-3 contract.
		fmt.Fprintf(os.Stderr, "::error::apiserver unreachable or Applications query failed: %v\n", err)
		return 3
	}
	code := convergenceExit(r, crdPresent, failOnUnhealthy)
	return code
}

// convergenceExit is the pure exit-code decision (unit-tested): the report's
// verdict when the CRD is present, in-progress (2) pre-bootstrap, and report-only
// suppression to 0. It also prints the report as a side effect so the CronWorkflow
// / job log carries the verdict.
func convergenceExit(r health.Report, crdPresent, failOnUnhealthy bool) int {
	code := health.InProgress.ExitCode() // pre-bootstrap: Application CRD not registered
	if crdPresent {
		printConvergenceReport(r)
		code = r.ExitCode()
	} else {
		fmt.Fprintln(os.Stderr, "convergence: Application CRD not present — pre-bootstrap (in-progress).")
	}
	if !failOnUnhealthy && code != 0 {
		fmt.Fprintf(os.Stderr, "::notice::health-incluster exit %d suppressed (--fail-on-unhealthy=false, report-only)\n", code)
		return 0
	}
	return code
}

func printConvergenceReport(r health.Report) {
	if len(r.Failed) == 0 && len(r.Pending) == 0 {
		fmt.Println("convergence: OK — all Argo Applications converged")
		return
	}
	for _, m := range r.Failed {
		fmt.Printf("  FAIL     %s\n", m)
	}
	for _, m := range r.Pending {
		fmt.Printf("  PENDING  %s\n", m)
	}
	fmt.Printf("convergence: %d hard-failed, %d in-progress\n", len(r.Failed), len(r.Pending))
}
