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
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func ciAssertHealthWorkflowCmd() *cobra.Command {
	var namespace, template string
	var timeout, interval int
	c := &cobra.Command{
		Use:   "assert-health-workflow",
		Short: "fail unless a Workflow submitted from the llz-cluster-health WorkflowTemplate Succeeds (the day-2 component RUN-path e2e gate)",
		Long: "Submits a one-shot Argo Workflow from the llz-cluster-health WorkflowTemplate\n" +
			"(fail-on-unhealthy=true, gate mode) and waits for it to Succeed — proving the\n" +
			"day-2 clusterHealthWorkflow component RUNS end-to-end: the signed llz image\n" +
			"passes the kyverno signature policy, the SA + executor RBAC authorize the run,\n" +
			"and `llz ci health-incluster` exits clean on the converged cluster. SKIPS (exit\n" +
			"0) if the WorkflowTemplate is absent — the component is DefaultDisabled, so this\n" +
			"is inert unless an env (the release-e2e gate) enabled it. Exit 0 succeeded/\n" +
			"skipped, 1 on Failed/Error/timeout.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			os.Exit(runCIAssertHealthWorkflow(namespace, template,
				time.Duration(timeout)*time.Second, time.Duration(interval)*time.Second))
			return nil
		},
	}
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

// submitHealthWorkflowFn creates the one-shot Workflow (kubectl create -f - -o
// json, manifest over stdin) and returns the created object's JSON. Seamed for
// tests. Interactive-style call site (pipes stdin), like firewallKubectlFn.
var submitHealthWorkflowFn = func(namespace, manifest string) ([]byte, error) {
	cmd := exec.Command("kubectl", "-n", namespace, "create", "-f", "-", "-o", "json")
	cmd.Stdin = strings.NewReader(manifest)
	return cmd.Output()
}

func runCIAssertHealthWorkflow(namespace, template string, timeout, interval time.Duration) int {
	// Inert on a normal instance: the component is DefaultDisabled, so the
	// WorkflowTemplate won't exist. Skip (exit 0) rather than fail — this verb
	// only means something where an env explicitly enabled the component.
	if !kExists("-n", namespace, "get", "workflowtemplate", template) {
		fmt.Printf("::notice::assert-health-workflow: WorkflowTemplate %s/%s not found — clusterHealthWorkflow component disabled; skipping.\n", namespace, template)
		return 0
	}

	out, err := submitHealthWorkflowFn(namespace, healthWorkflowManifest(template, namespace))
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::assert-health-workflow: could not submit Workflow from %s/%s: %v\n", namespace, template, err)
		return 1
	}
	name, ok := createdWorkflowName(out)
	if !ok {
		fmt.Fprintf(os.Stderr, "::error::assert-health-workflow: submitted Workflow but could not read its name from the create response\n")
		return 1
	}
	fmt.Printf("assert-health-workflow: submitted Workflow %s/%s from WorkflowTemplate %s; waiting up to %s for it to Succeed…\n", namespace, name, template, timeout)

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
				fmt.Printf("assert-health-workflow: Workflow %s/%s Succeeded.\n", namespace, name)
				return 0
			}
			fmt.Fprintf(os.Stderr, "::error::assert-health-workflow: Workflow %s/%s reached phase %q (not Succeeded).\n", namespace, name, phase)
			fmt.Fprint(os.Stderr, execCombined("kubectl", "-n", namespace, "get", "workflow", name, "-o", "yaml"))
			return 1
		}
		if time.Now().After(deadline) {
			shown := phase
			if shown == "" {
				shown = "<none yet>"
			}
			fmt.Fprintf(os.Stderr, "::error::assert-health-workflow: Workflow %s/%s did not reach a terminal phase within %s (last phase %s).\n", namespace, name, timeout, shown)
			fmt.Fprint(os.Stderr, execCombined("kubectl", "-n", namespace, "get", "workflow", name, "-o", "yaml"))
			return 1
		}
		time.Sleep(interval)
	}
}
