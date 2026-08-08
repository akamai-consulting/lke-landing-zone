package converge

// cobra_wait.go — the CLI surface for wait.
//
// Split from wait.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func WaitPodsCmd() *cobra.Command {
	var ns, phase string
	var timeout, interval int
	c := &cobra.Command{
		Use:   "wait-pods <pod>...",
		Short: "wait for named pods to reach a status phase (default Running)",
		Long: "Native port of the 'Wait for OpenBao pods to be running' loop in\n" +
			"llz-bootstrap-openbao.yml. Watches each named pod with `kubectl wait`\n" +
			"(--for=create to ride out a not-yet-created pod, then\n" +
			"--for=jsonpath={.status.phase}=<phase>), under one shared --timeout. Phase\n" +
			"(not Readiness) on purpose: a pod can stay unready until a later step acts on\n" +
			"it (OpenBao pods are unready until unsealed), so a readiness wait would\n" +
			"deadlock a first bootstrap. On timeout it dumps the namespace's workloads,\n" +
			"the stuck pod's describe, and recent events (combined stdout+stderr, so an\n" +
			"empty namespace or a NotFound still surfaces the reason), then exits 1.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, pods []string) error {
			cmd.SilenceUsage = true
			return runCIWaitPods(ns, phase, pods, timeout, interval)
		},
	}
	c.Flags().StringVar(&ns, "namespace", "", "namespace of the pods (required)")
	c.Flags().StringVar(&phase, "phase", "Running", "status phase to wait for")
	c.Flags().IntVar(&timeout, "timeout", 600, "total wait budget in seconds, shared across all pods")
	c.Flags().IntVar(&interval, "interval", 5, "seconds between polls")
	return c
}
func WaitClusterReadyCmd() *cobra.Command {
	var timeout, interval, requestTimeout, expectNodes int
	var tfvarsPath string
	c := &cobra.Command{
		Use:   "wait-cluster-ready",
		Short: "wait until the apiserver answers AND the expected node count is Ready under $KUBECONFIG",
		Long: "Native port of the post-rotation health gate loop in llz-secret-rotation.yml\n" +
			"and the 'Wait for cluster API ready' loop in llz-terraform.yml. Polls\n" +
			"`kubectl get nodes` until (a) the control plane accepts the credentials and\n" +
			"(b) at least --expect-nodes nodes report Ready=True. A fresh LKE pool is\n" +
			"created in seconds (the API returns) but its nodes take minutes to register\n" +
			"and go Ready; gating only on apiserver reachability lets bootstrap proceed\n" +
			"onto an empty pool, where the apl-operator pod (and then helm_release.apl)\n" +
			"sits Pending until it times out. Requiring nodes Ready closes that gap.\n" +
			"With --tfvars the expected count is read from that file's node_count\n" +
			"(overriding --expect-nodes when > 0; autoscaler/absent → falls back to\n" +
			"--expect-nodes). On timeout it dumps node readiness and probes the\n" +
			"apiserver's /version directly so 'API never came up', 'API up but ACL\n" +
			"blocks this runner', and 'API up, nodes never joined' are distinguishable.\n" +
			"Exit 0 ready, 1 on timeout.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIWaitClusterReady(timeout, interval, requestTimeout, resolveExpectNodes(tfvarsPath, expectNodes))
		},
	}
	c.Flags().IntVar(&timeout, "timeout", 360, "total wait budget in seconds")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between polls")
	c.Flags().IntVar(&requestTimeout, "request-timeout", 10, "kubectl --request-timeout per poll, in seconds (bounds a hanging apiserver)")
	c.Flags().IntVar(&expectNodes, "expect-nodes", 1, "minimum number of Ready nodes to wait for")
	c.Flags().StringVar(&tfvarsPath, "tfvars", "", "cluster <region>.tfvars path; its node_count (when > 0) sets the expected Ready-node count")
	return c
}
