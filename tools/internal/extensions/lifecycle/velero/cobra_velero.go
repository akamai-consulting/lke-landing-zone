package velero

// cobra_velero.go — the CLI surface. Flag wiring and help text only.

import (
	"context"

	"github.com/spf13/cobra"
)

// DrillCmd is `llz ci assert-velero-restore`.
func DrillCmd() *cobra.Command {
	var region string
	var drill bool
	c := &cobra.Command{
		Use:   "assert-velero-restore",
		Short: "prove Velero's backup lands encrypted in its bucket; with --drill (e2e only), back up, delete and restore a canary namespace",
		Long: "The gate for the velero component. On any deployment that enables it: checks the LIVE\n" +
			"Schedule still excludes llz-openbao and llz-velero, waits for the BackupStorageLocation to be\n" +
			"Available, takes a backup from the Schedule's own template and requires it Completed with items\n" +
			"and no errors, reads its tarball's presence back from the velero bucket through a temporary\n" +
			"read-only key and requires a GET without the SSE-C key to be refused (the object is ciphertext),\n" +
			"then requires Prometheus to scrape Velero and load its alert rules. bootstrap-openbao runs this\n" +
			"after converge, so a Velero that cannot back up fails the bootstrap that installed it.\n" +
			"\n" +
			"--drill adds the round trip, and is refused on any --region but the e2e lane's own deployments\n" +
			"(e2e, e2e-<forge>): writes a ConfigMap and a Secret into the llz-velero-drill namespace, backs\n" +
			"that namespace up, deletes it, restores it, and fails unless both come back with the nonce and\n" +
			"the restore's label. It touches only that namespace; it never restores over anything else.\n" +
			"\n" +
			"Needs LINODE_API_TOKEN. Skips cleanly when spec.components.velero is disabled for --region.\n" +
			"See docs/runbooks/velero-dr.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return Run(context.Background(), region, drill)
		},
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) to check (required)")
	c.Flags().BoolVar(&drill, "drill", false, "also back up, delete and restore a canary namespace (e2e lane deployments only)")
	return c
}
