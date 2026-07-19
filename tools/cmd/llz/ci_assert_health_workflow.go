package main

// ci_assert_health_workflow.go — `llz ci assert-health-workflow`: the e2e gate
// that proves the day-2 clusterHealthWorkflow component actually RUNS, not just
// that its manifests synced. Enabling the component makes converge validate the
// DEPLOY path (Kyverno admits the WorkflowTemplate/CronWorkflow/RBAC CRs, Argo
// reconciles them); this verb validates the RUN path — it submits a one-shot
// Workflow from the llz-cluster-health WorkflowTemplate and asserts it Succeeds.
// That exercises everything a scheduled CronWorkflow would: the emissary pulls
// the signed llz image (kyverno-verify-llz-image-signature gates the pod), the
// llz-cluster-health SA + its Role/executor-RBAC authorize the run, and the
// `llz ci health-incluster` verb executes to a clean exit on the converged
// cluster (docs/designs/day2-incluster-health.md).
//
// kubectl-based (runs in the release-e2e converge job, which already holds
// cluster access) — the same treatment as assert-loki/assert-scrape-targets. It
// SKIPS gracefully (exit 0) when the WorkflowTemplate is absent, so it is inert
// on a normal instance where the component is DefaultDisabled and only asserts
// where the e2e enabled it. Exit 0 succeeded/skipped, 1 otherwise.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/spf13/cobra"
)

func ciAssertHealthWorkflowCmd() *cobra.Command {
	var region, namespace, template string
	var timeout, interval int
	c := &cobra.Command{
		Use:   "assert-health-workflow",
		Short: "fail unless a Workflow submitted from the llz-cluster-health WorkflowTemplate Succeeds (the day-2 component RUN-path e2e gate)",
		Long: "Submits a one-shot Argo Workflow from the llz-cluster-health WorkflowTemplate\n" +
			"(fail-on-unhealthy=true, gate mode) and waits for it to Succeed — proving the\n" +
			"day-2 clusterHealthWorkflow component RUNS end-to-end: the signed llz image\n" +
			"passes the kyverno signature policy, the SA + executor RBAC authorize the run,\n" +
			"and `llz ci health-incluster` exits clean on the converged cluster.\n\n" +
			"Skipping is anchored to the SPEC, not the cluster: with --region set, it skips\n" +
			"only when spec.components.clusterHealthWorkflow is disabled for that env. If the\n" +
			"component IS enabled and the WorkflowTemplate is absent, that is a deploy\n" +
			"failure (a render regression, a Kyverno denial on the CR, an unsynced Argo app)\n" +
			"and this FAILS — the cluster is the thing under test, so it cannot also be the\n" +
			"thing that decides whether to test. Without --region it falls back to skipping\n" +
			"on an absent template, for ad-hoc runs outside an instance checkout.\n\n" +
			"Exit 0 succeeded/skipped, 1 on Failed/Error/timeout.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertHealthWorkflow(region, namespace, template,
				time.Duration(timeout)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&region, "region", "", "deployment whose spec decides if the component is expected (empty falls back to skipping on an absent WorkflowTemplate)")
	c.Flags().StringVar(&namespace, "namespace", "llz-argo-workflows", "namespace the WorkflowTemplate + submitted Workflow live in")
	c.Flags().StringVar(&template, "template", "llz-cluster-health", "WorkflowTemplate to submit a one-shot Workflow from")
	c.Flags().IntVar(&timeout, "timeout", 300, "seconds to wait for the submitted Workflow to reach a terminal phase before failing")
	c.Flags().IntVar(&interval, "interval", 10, "seconds between phase polls")
	return c
}

// healthWorkflowManifest builds the one-shot Workflow that references the given
// WorkflowTemplate in gate mode (fail-on-unhealthy=true — a genuinely unhealthy
// cluster fails the run, which is the signal we want post-converge). generateName
// lets the apiserver mint a unique name so repeated runs never collide. Pure.
func healthWorkflowManifest(template, namespace string) string {
	m := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Workflow",
		"metadata": map[string]any{
			"generateName": "e2e-assert-health-",
			"namespace":    namespace,
			"labels":       map[string]any{"app.kubernetes.io/managed-by": "llz-ci-assert"},
		},
		"spec": map[string]any{
			"workflowTemplateRef": map[string]any{"name": template},
			"arguments": map[string]any{
				"parameters": []any{
					map[string]any{"name": "fail-on-unhealthy", "value": "true"},
				},
			},
		},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// createdWorkflowName extracts .metadata.name from `kubectl create -o json`
// output (the apiserver-assigned name for a generateName object). Pure.
func createdWorkflowName(raw []byte) (string, bool) {
	var doc struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Metadata.Name == "" {
		return "", false
	}
	return doc.Metadata.Name, true
}

// workflowPhase extracts .status.phase from a Workflow's JSON. Empty until Argo
// writes a phase (a just-created Workflow has none yet). Pure.
func workflowPhase(raw []byte) string {
	var doc struct {
		Status struct {
			Phase   string `json:"phase"`
			Message string `json:"message"`
		} `json:"status"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return doc.Status.Phase
}

// classifyWorkflowPhase maps an Argo Workflow phase to (terminal, succeeded).
// Succeeded → done+ok; Failed/Error → done+!ok; Running/Pending/"" → not done. Pure.
func classifyWorkflowPhase(phase string) (terminal, succeeded bool) {
	switch phase {
	case "Succeeded":
		return true, true
	case "Failed", "Error":
		return true, false
	default:
		return false, false
	}
}

// healthVerdictRe matches the health run's convergence verdict line, e.g.
// "convergence: 0 hard-failed, 9 in-progress".
var healthVerdictRe = regexp.MustCompile(`convergence:\s*(\d+) hard-failed,\s*(\d+) in-progress`)

// healthTransientOnly reports whether a FAILED health run's logs show a purely
// TRANSIENT state: zero hard failures with work still in progress (e.g. the
// argocd-redis WRONGPASS auth split flapping apps to Unknown right after
// converge — observed live: "0 hard-failed, 9 in-progress"). That is a cluster
// mid-settle, not an unhealthy cluster; the caller retries instead of failing
// the gate. Anything else (hard failures, no verdict line) is NOT transient.
func healthTransientOnly(logs string) bool {
	m := healthVerdictRe.FindStringSubmatch(logs)
	if m == nil {
		return false
	}
	return m[1] == "0" && m[2] != "0"
}

// Health-gate retry knobs: a transient-only failure is retried with a fresh
// Workflow after a pause (package vars so tests shrink them).
var (
	healthRetryAttempts = 3
	healthRetryPause    = 90 * time.Second
)

// submitHealthWorkflowFn creates the one-shot Workflow (kubectl create -f - -o
// json, manifest over stdin) and returns the created object's JSON. Seamed for
// tests. Interactive-style call site (pipes stdin), like firewallKubectlFn.
var submitHealthWorkflowFn = func(namespace, manifest string) ([]byte, error) {
	cmd := exec.Command("kubectl", "-n", namespace, "create", "-f", "-", "-o", "json")
	cmd.Stdin = strings.NewReader(manifest)
	return cmd.Output()
}

// runCIAssertHealthWorkflow returns nil on Succeeded/skipped and an error on
// Failed/Error/timeout (cobra exits 1 on it). The ::error:: / ::notice::
// annotations stay as direct writes: GitHub parses an annotation only at the
// start of a line, and a returned error reaches stderr behind main.go's "llz: ".
func runCIAssertHealthWorkflow(region, namespace, template string, timeout, interval time.Duration) error {
	// Whether to skip is a question about the SPEC, not the cluster. Anchoring it
	// to the cluster made the gate unfalsifiable: the e2e explicitly enables
	// clusterHealthWorkflow, so a WorkflowTemplate that never synced — a render
	// regression, a Kyverno denial on the CR, a wedged Argo app — read as
	// "component disabled" and passed green, proving nothing. Nothing else asserts
	// the template SHOULD exist. Same anchoring assert-broad-pat-rotation uses.
	if !kExists("-n", namespace, "get", "workflowtemplate", template) {
		expected, why := healthWorkflowExpected(region)
		if expected {
			fmt.Fprintf(os.Stderr, "::error::assert-health-workflow: WorkflowTemplate %s/%s is MISSING but %s. The component did not deploy — check the Argo app that owns it and whether admission denied the CR.\n",
				namespace, template, why)
			return fmt.Errorf("assert-health-workflow: WorkflowTemplate %s/%s is MISSING but %s", namespace, template, why)
		}
		fmt.Printf("::notice::assert-health-workflow: WorkflowTemplate %s/%s not found and %s; skipping.\n", namespace, template, why)
		return nil
	}

	// Reap any prior e2e probe Workflows first: on a REUSED cluster a Failed one
	// lingers and (even with converge now ignoring them) is just noise. Best-effort.
	execCombined("kubectl", "-n", namespace, "delete", "workflow",
		"-l", "workflows.argoproj.io/workflow-template="+template, "--field-selector=status.phase!=Running", "--ignore-not-found")

	for attempt := 1; ; attempt++ {
		out, err := submitHealthWorkflowFn(namespace, healthWorkflowManifest(template, namespace))
		if err != nil {
			fmt.Fprintf(os.Stderr, "::error::assert-health-workflow: could not submit Workflow from %s/%s: %v\n", namespace, template, err)
			return fmt.Errorf("assert-health-workflow: could not submit Workflow from %s/%s: %w", namespace, template, err)
		}
		name, ok := createdWorkflowName(out)
		if !ok {
			fmt.Fprintf(os.Stderr, "::error::assert-health-workflow: submitted Workflow but could not read its name from the create response\n")
			return fmt.Errorf("assert-health-workflow: submitted Workflow but could not read its name from the create response")
		}
		fmt.Printf("assert-health-workflow: submitted Workflow %s/%s from WorkflowTemplate %s (attempt %d/%d); waiting up to %s for it to Succeed…\n",
			namespace, name, template, attempt, healthRetryAttempts, timeout)

		switch waitHealthWorkflow(namespace, name, timeout, interval) {
		case healthOK:
			fmt.Printf("assert-health-workflow: Workflow %s/%s Succeeded.\n", namespace, name)
			return nil
		case healthFailed:
			// A failed run whose own verdict is "0 hard-failed, N in-progress" is a
			// cluster MID-SETTLE (e.g. the argocd-redis WRONGPASS flap right after an
			// operator roll), not an unhealthy cluster — retry with a fresh Workflow.
			logs := execCombined("kubectl", "-n", namespace, "logs",
				"-l", "workflows.argoproj.io/workflow="+name, "-c", "main", "--tail=-1")
			if attempt < healthRetryAttempts && healthTransientOnly(logs) {
				fmt.Printf("::warning::assert-health-workflow: %s/%s failed with 0 hard-failed (in-progress only — cluster still settling); retrying in %s…\n",
					namespace, name, healthRetryPause)
				execCombined("kubectl", "-n", namespace, "delete", "workflow", name, "--ignore-not-found")
				time.Sleep(healthRetryPause)
				continue
			}
			fmt.Fprintf(os.Stderr, "::error::assert-health-workflow: Workflow %s/%s failed (not Succeeded).\n", namespace, name)
			fmt.Fprintln(os.Stderr, logs)
			fmt.Fprint(os.Stderr, execCombined("kubectl", "-n", namespace, "get", "workflow", name, "-o", "yaml"))
			return fmt.Errorf("assert-health-workflow: Workflow %s/%s failed (not Succeeded)", namespace, name)
		case healthTimeout:
			fmt.Fprintf(os.Stderr, "::error::assert-health-workflow: Workflow %s/%s did not reach a terminal phase within %s.\n", namespace, name, timeout)
			fmt.Fprint(os.Stderr, execCombined("kubectl", "-n", namespace, "get", "workflow", name, "-o", "yaml"))
			return fmt.Errorf("assert-health-workflow: Workflow %s/%s did not reach a terminal phase within %s", namespace, name, timeout)
		}
	}
}

// healthWaitResult is waitHealthWorkflow's terminal classification.
type healthWaitResult int

const (
	healthOK healthWaitResult = iota
	healthFailed
	healthTimeout
)

// waitHealthWorkflow polls one submitted Workflow to a terminal phase.
func waitHealthWorkflow(namespace, name string, timeout, interval time.Duration) healthWaitResult {
	deadline := time.Now().Add(timeout)
	for {
		// `kubectl get workflow <name> -o json` returns the BARE object, not a
		// List — so parse it directly. (kItems parses `.items`, which a single-named
		// get never has, so it always yielded nothing → phase stuck at "<none yet>"
		// and the poll timed out even on a workflow that had already Succeeded.)
		var phase string
		if out, err := execOutput("kubectl", "-n", namespace, "get", "workflow", name, "-o", "json"); err == nil {
			phase = workflowPhase(out)
		}
		terminal, succeeded := classifyWorkflowPhase(phase)
		if terminal {
			if succeeded {
				return healthOK
			}
			return healthFailed
		}
		if time.Now().After(deadline) {
			return healthTimeout
		}
		time.Sleep(interval)
	}
}

// healthWorkflowExpected reports whether spec.components.clusterHealthWorkflow is
// enabled for region — i.e. whether an absent WorkflowTemplate is a failure or a
// legitimate no-op — along with the reason, for the operator-facing message.
//
// An unreadable spec is treated as NOT expected: the fallback must not turn
// ad-hoc runs outside an instance checkout into failures. That keeps one hole
// (no --region, no spec) but it is now explicit and stated in the output,
// instead of being the default for every caller.
func healthWorkflowExpected(region string) (bool, string) {
	if region == "" {
		return false, "no --region was given, so the spec could not say whether the component is expected"
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return false, fmt.Sprintf("the instance spec could not be read (%v), so the component's expected state is unknown", err)
	}
	e, ok := lz.Env(region)
	if !ok {
		return false, fmt.Sprintf("%q is not a deployment in the instance spec", region)
	}
	if clusterspec.ComponentEnabled(e.Components, "clusterHealthWorkflow") {
		return true, fmt.Sprintf("spec.components.clusterHealthWorkflow IS enabled for %s", region)
	}
	return false, fmt.Sprintf("spec.components.clusterHealthWorkflow is disabled for %s", region)
}
