package assertplatform

// cobra_healthworkflow.go — the CLI surface for healthworkflow.
//
// Split from healthworkflow.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"time"

	"github.com/spf13/cobra"
)

func HealthWorkflowCmd() *cobra.Command {
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
