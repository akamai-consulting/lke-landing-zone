package main

// ci_wait_apl_pipeline.go implements `llz ci wait-apl-pipeline` — the native
// port of null_resource.apl_pipeline_ready's local-exec heredoc in
// instance-template cluster-bootstrap/main.tf. It is the readiness gate that
// blocks downstream TF resources (the platform-bootstrap Application + AppProject
// and the Kyverno race policies) until apl-operator's helmfile has brought the
// platform prerequisites up. helm_release.apl's own wait only covers the
// apl-operator Deployment; the helmfile then installs ~40 components over 10-15
// minutes, and everything TF does afterward races that pipeline.
//
// WHAT IT WAITS FOR — and WHY each is a "real readiness" signal (not just "CRD
// present" or "Deployment created"):
//   1. Argo CD application-controller StatefulSet readyReplicas=1 — argocd can
//      accept Applications. The CRD is Established ~60-90s before the controller
//      actually serves, so gating on the CRD alone is too weak. StatefulSets
//      expose no Ready condition (.status.conditions stays empty even at
//      readyReplicas=1), so `--for=condition=Ready` never returns — gate on
//      readyReplicas via a jsonpath --for clause instead.
//   2. Kyverno admission-controller Deployment Available — the mutating-webhook
//      backend is reachable: CRD registered AND webhook config installed AND
//      kyverno-svc has Ready endpoints, in one wait.
//   3. cert-manager webhook Deployment Available — Argo's first sync applies
//      Certificate CRs (openbao-tls, harbor-tls, …); before the validating
//      webhook is up they 503 with "failed calling webhook".
//
// Each stage gates the CRD Established first, then the serving workload. Unlike
// `llz ci apply-kyverno-policy`, this gate FAILS LOUD (convergence contract): a
// stage that never becomes ready returns a non-nil error so `terraform apply`
// fails, rather than soft-failing to ::warning:: + exit 0 — soft-fail-and-
// continue is how cluster bootstraps declare success while half-broken.
//
// DELIBERATELY NOT here:
//   - ESO stage: external-secrets is not installed by apl-core; the
//     platform-bootstrap tree installs it (Argo sync-wave -15), and that tree's
//     root Application depends_on THIS gate — so waiting for ESO here is
//     circular (the gate would block the resource that installs ESO and hang its
//     existence-poll on the never-appearing CRD). Argo's own wave ordering +
//     SkipDryRunOnMissingResource handles operator-before-consumers instead.
//   - gitea/otomi-values self-heal: otomi.git points at the external GitHub
//     repo, so apl-operator's values tree comes over HTTPS, not gitea-http — the
//     old DNS race is gone. (Gitea is DISABLED on v6; apl-core's gitops-global
//     app clones otomi.git.repoUrl — the external repo, not gitea — and converges
//     Synced/Healthy, verified on v6 e2e. The convergence health allowlist still
//     defers it as a conservative no-op; see allowlists.go for the evidence.)
//
// The poll/wait state machine is driven through injected seams (kubectl runner,
// clock, sleep) so it is unit-tested without a cluster.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// aplWaitStage is one (existence-poll → condition-wait) step.
type aplWaitStage struct {
	desc        string        // human label for the progress line
	namespace   string        // "" for cluster-scoped
	resource    string        // e.g. "crd/applications.argoproj.io"
	forClause   string        // bare condition ("Established"/"Available") OR a full --for clause ("jsonpath=…=1")
	existBudget time.Duration // existence-poll deadline
	condTimeout string        // kubectl wait --timeout value, e.g. "5m"
}

// aplPipelineStages — the platform prerequisites apl-operator's helmfile brings
// up, in order. Each CRD is gated Established first, then its serving workload
// for real readiness.
//
// BUDGETS ARE SIZED FROM MEASUREMENT, not inherited from the bash this ports.
// Those numbers summed to 6600s (110 MINUTES) inside a job whose timeout-minutes
// is 70 — so no stage could ever report its own timeout on a genuinely slow run;
// GitHub's job axe always landed first, with no verdict and no diagnostics. The
// file's own comment sized the job at "worst-case ~25m", already 4.4x under its
// own budget.
//
// Measured on a passing cold e2e (run 29658429694): the whole "Bootstrap cluster"
// step — apl-core install plus all six stages below — took 342s (5m42s).
//
// Stage 1 keeps the largest existence budget because it is the load-bearing one:
// it waits for apl-operator's helmfile to START producing anything. Once the
// first CRD lands the rest follow within a minute, so their inherited 900s/600s
// ceilings were pure padding. Worst case is now 3300s (55m) — still ~10x the
// measured value, but inside the job timeout, so a hung stage FAILS with the
// message it was written to print.
func aplPipelineStages() []aplWaitStage {
	return []aplWaitStage{
		{"Argo CD CRD", "", "crd/applications.argoproj.io", "Established", 600 * time.Second, "3m"},
		{"Argo CD application-controller", "argocd", "statefulset/argocd-application-controller", "jsonpath={.status.readyReplicas}=1", 300 * time.Second, "5m"},
		{"Kyverno CRD", "", "crd/clusterpolicies.kyverno.io", "Established", 300 * time.Second, "3m"},
		{"Kyverno admission-controller", "kyverno", "deployment/kyverno-admission-controller", "Available", 300 * time.Second, "3m"},
		{"cert-manager CRD", "", "crd/certificates.cert-manager.io", "Established", 300 * time.Second, "3m"},
		{"cert-manager webhook", "cert-manager", "deployment/cert-manager-webhook", "Available", 300 * time.Second, "3m"},
	}
}

// aplGateDeps are the seams the gate drives: one kubectl invocation (KUBECONFIG
// already wired) returning combined output + whether it exited 0, plus now/sleep
// for the testable deadline loop.
type aplGateDeps struct {
	kubectl func(args ...string) (string, bool)
	now     func() time.Time
	sleep   func(time.Duration)
}

func ciWaitAplPipelineCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "wait-apl-pipeline",
		Short: "block until apl-operator's helmfile brings argocd/kyverno/cert-manager up (terraform local-exec body)",
		Long: "Native port of null_resource.apl_pipeline_ready's local-exec heredoc. Writes\n" +
			"KUBECONFIG_RAW to a tempfile, then for each platform prerequisite polls until\n" +
			"the resource EXISTS (kubectl wait errors immediately on NotFound) and waits\n" +
			"for its real-readiness condition: Argo CD application-controller\n" +
			"(readyReplicas), the Kyverno admission controller (Available), and the\n" +
			"cert-manager webhook (Available). FAILS LOUD on any timeout (convergence\n" +
			"contract — no soft-fail), dumping apl-operator pods + logs when a resource\n" +
			"never appears. Reads KUBECONFIG_RAW.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIWaitAplPipeline() },
	}
}

func runCIWaitAplPipeline() error {
	raw := os.Getenv("KUBECONFIG_RAW")
	if raw == "" {
		return fmt.Errorf("KUBECONFIG_RAW must be set")
	}
	kubeconfig, cleanup, err := writeTempKubeconfig("llz-apl-pipeline-kubeconfig-*", []byte(raw))
	if err != nil {
		return err
	}
	defer cleanup()

	return waitAplPipeline(aplPipelineStages(), newAplGateDepsFor(kubeconfig))
}

func waitAplPipeline(stages []aplWaitStage, d aplGateDeps) error {
	for _, s := range stages {
		fmt.Printf("Waiting for %s (%s)...\n", s.desc, s.resource)
		if err := waitForAplResource(d, s); err != nil {
			return err
		}
	}
	fmt.Println("apl-operator pipeline is ready — downstream TF resources can proceed.")
	return nil
}

// waitForAplResource polls for the resource to EXIST, then waits for its
// readiness condition. An existence timeout dumps apl-operator diagnostics;
// both an existence timeout and a condition-wait failure are hard errors.
func waitForAplResource(d aplGateDeps, s aplWaitStage) error {
	var nsArgs []string
	if s.namespace != "" {
		nsArgs = []string{"-n", s.namespace}
	}
	// Poll for existence on a 10s cadence (mirrors the bash `until kubectl get …
	// sleep 10` loop) before handing off to the condition wait.
	if !pollUntil(d.now, d.sleep, s.existBudget, 10*time.Second, func() bool {
		_, ok := d.kubectl(append(append([]string{}, nsArgs...), "get", s.resource)...)
		return ok
	}) {
		fmt.Fprintf(os.Stderr, "::error::%s did not appear within %s — apl-operator helmfile likely stalled.\n", s.resource, s.existBudget)
		dumpAplOperatorDiagnostics(d)
		return fmt.Errorf("%s did not appear within %s", s.resource, s.existBudget)
	}
	// Bare condition name → `--for=condition=<name>`; a clause with "=" (jsonpath)
	// is passed verbatim. Matches the bash `case "$3" in *=*) … esac`.
	forArg := "condition=" + s.forClause
	if strings.Contains(s.forClause, "=") {
		forArg = s.forClause
	}
	waitArgs := append(append([]string{}, nsArgs...), "wait", "--for="+forArg, s.resource, "--timeout="+s.condTimeout)
	if out, ok := d.kubectl(waitArgs...); !ok {
		fmt.Fprint(os.Stderr, out)
		return fmt.Errorf("%s did not reach %q within %s", s.resource, s.forClause, s.condTimeout)
	}
	return nil
}

// dumpAplOperatorDiagnostics prints apl-operator pods + recent operator logs to
// stderr (best-effort) so an existence timeout shows WHY the helmfile stalled.
func dumpAplOperatorDiagnostics(d aplGateDeps) {
	if out, _ := d.kubectl("-n", "apl-operator", "get", "pods"); out != "" {
		fmt.Fprintln(os.Stderr, strings.TrimRight(out, "\n"))
	}
	if out, _ := d.kubectl("-n", "apl-operator", "logs", "deploy/apl-operator", "--tail=80"); out != "" {
		fmt.Fprintln(os.Stderr, strings.TrimRight(out, "\n"))
	}
}
