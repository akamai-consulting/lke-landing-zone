package converge

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
)

// healthInClusterExitCode builds the in-cluster client, computes the convergence
// report, prints it, and returns the exit code. apiserver-unreachable → 3.
// The in-cluster client is built by the ConvergenceReport seam rather than here:
// its construction and its use are one capability, and splitting them would put
// the reconciler's client model in this package's imports for no gain.
func healthInClusterExitCode(ctx context.Context, failOnUnhealthy bool) int {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	r, crdPresent, err := deps.ConvergenceReport(cctx)
	if err != nil {
		// A query failure here is apiserver-unreachable-class: exit 3 (transient),
		// not a cluster hard-fail — matching `llz ci health`'s exit-3 contract.
		fmt.Fprintf(os.Stderr, "::error::apiserver unreachable or Applications query failed: %v\n", err)
		return 3
	}
	// Print the verdict to the job log, then decide the exit code.
	if crdPresent {
		printConvergenceReport(r)
	} else {
		fmt.Fprintln(os.Stderr, "convergence: Application CRD not present — pre-bootstrap (in-progress).")
	}
	code := ConvergenceExit(r, crdPresent, failOnUnhealthy)
	if !failOnUnhealthy && code != 0 {
		fmt.Fprintf(os.Stderr, "::notice::health-incluster exit %d suppressed (--fail-on-unhealthy=false, report-only)\n", code)
	}
	return code
}

// ConvergenceExit is the PURE exit-code decision (unit-tested, no I/O): the
// report's verdict when the Application CRD is present, in-progress (2)
// pre-bootstrap, and report-only suppression to 0. Exit 3 (apiserver unreachable)
// is handled by the caller before this — report-only does NOT suppress it.
// EXPORTED because a coupling test spans the extraction boundary:
// TestHealthInClusterGateRejectsEmptyCorpus drives the RECONCILER's
// convergenceReport (package main) into THIS package's exit-code contract, and
// that pairing is the thing worth testing — the two halves must agree that an
// empty Applications corpus is in-progress rather than converged.
func ConvergenceExit(r health.Report, crdPresent, failOnUnhealthy bool) int {
	code := health.InProgress.ExitCode() // pre-bootstrap: Application CRD not registered
	if crdPresent {
		code = r.ExitCode()
	}
	if !failOnUnhealthy && code != 0 {
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
