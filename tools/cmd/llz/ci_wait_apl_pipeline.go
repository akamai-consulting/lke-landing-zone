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
//     app still hardwires a gitea clone and fails "no such host", which the
//     convergence health allowlist defers so it can't pin the gate.)
//
// The poll/wait state machine is driven through injected seams (kubectl runner,
// clock, sleep) so it is unit-tested without a cluster.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
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
// up, in order; budgets/timeouts match the bash this ports. Each CRD is gated
// Established first, then its serving workload for real readiness.
func aplPipelineStages() []aplWaitStage {
	return []aplWaitStage{
		{"Argo CD CRD", "", "crd/applications.argoproj.io", "Established", 900 * time.Second, "5m"},
		{"Argo CD application-controller", "argocd", "statefulset/argocd-application-controller", "jsonpath={.status.readyReplicas}=1", 600 * time.Second, "10m"},
		{"Kyverno CRD", "", "crd/clusterpolicies.kyverno.io", "Established", 900 * time.Second, "5m"},
		{"Kyverno admission-controller", "kyverno", "deployment/kyverno-admission-controller", "Available", 600 * time.Second, "5m"},
		{"cert-manager CRD", "", "crd/certificates.cert-manager.io", "Established", 900 * time.Second, "5m"},
		{"cert-manager webhook", "cert-manager", "deployment/cert-manager-webhook", "Available", 600 * time.Second, "5m"},
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
	kubeconfig, err := os.CreateTemp("", "llz-apl-pipeline-kubeconfig-*")
	if err != nil {
		return fmt.Errorf("create kubeconfig tempfile: %w", err)
	}
	defer os.Remove(kubeconfig.Name())
	if _, err := kubeconfig.WriteString(raw); err != nil {
		kubeconfig.Close()
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	kubeconfig.Close()

	d := aplGateDeps{
		kubectl: func(args ...string) (string, bool) {
			cmd := exec.Command("kubectl", args...)
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig.Name())
			var buf bytes.Buffer
			cmd.Stdout, cmd.Stderr = &buf, &buf
			return buf.String(), cmd.Run() == nil
		},
		now:   time.Now,
		sleep: time.Sleep,
	}
	return waitAplPipeline(aplPipelineStages(), d)
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
	if !pollAplExist(d, s.existBudget, func() bool {
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

// pollAplExist calls cond immediately, then every 10s until it returns true or
// the budget elapses (mirrors the bash `until kubectl get … sleep 10` loop).
func pollAplExist(d aplGateDeps, budget time.Duration, cond func() bool) bool {
	deadline := d.now().Add(budget)
	for {
		if cond() {
			return true
		}
		if !d.now().Before(deadline) {
			return false
		}
		d.sleep(10 * time.Second)
	}
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
