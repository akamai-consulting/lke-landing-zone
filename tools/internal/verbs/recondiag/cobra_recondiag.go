package recondiag

// cobra_recondiag.go — the `llz ci diagnose-reconciler` flag set. Flag wiring and
// help text, and nothing that decides anything.

import "github.com/spf13/cobra"

func DiagnoseReconcilerCmd() *cobra.Command {
	var ns string
	c := &cobra.Command{
		Use:   "diagnose-reconciler",
		Short: "dump llz-reconciler + apl-overlay lane diagnostics (best-effort, never fails)",
		Long: "Collects what `llz ci converge` tells you to look at when the obj chain does\n" +
			"not complete: the reconciler's pod state, ITS LOGS (every apl-overlay skip\n" +
			"prints there), the " + OverlayMetric + " scrape wiring, the leader lease,\n" +
			"apl-core's effective AplObjectStorage settings, and whether ESO ever built\n" +
			"loki-s3-linode-credentials.\n\n" +
			"Exists because converge's message said \"check llz-reconciler " + OverlayMetric + "\"\n" +
			"while nothing in the failure bundle contained it — so answering the question\n" +
			"the gate asked required a live cluster that had usually been torn down.\n" +
			"The effective AplObjectStorage is the decisive one: `provider.type: disabled`\n" +
			"alongside a present obj-secrets is a STALL, not a chain still settling.\n\n" +
			"Skips cleanly when $KUBECONFIG is absent/empty or the apiserver is\n" +
			"unreachable. Always exits 0: diagnostics must never mask the failure that\n" +
			"triggered them.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return Run(ns) },
	}
	c.Flags().StringVar(&ns, "namespace", Namespace, "namespace holding the llz-reconciler install")
	return c
}
