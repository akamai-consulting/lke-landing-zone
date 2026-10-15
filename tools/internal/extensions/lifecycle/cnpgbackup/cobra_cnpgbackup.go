package cnpgbackup

// cobra_cnpgbackup.go — the CLI surface. Flag wiring and help text only.

import (
	"context"

	"github.com/spf13/cobra"
)

// CheckCmd is `llz ci assert-cnpg-backup`.
func CheckCmd() *cobra.Command {
	var region string
	var drill bool
	c := &cobra.Command{
		Use:   "assert-cnpg-backup",
		Short: "prove apl-core's CNPG database backups land in the cnpg bucket; with --drill (e2e only), restore one into a scratch cluster",
		Long: "The gate for the cnpgBackup component. On any deployment that enables it, for each database the\n" +
			"spec backs up (keycloak, and harbor where Harbor runs): waits for the LIVE Cluster to carry the\n" +
			"barman-cloud plugin stanza (refusing the in-tree path, which LLZ's alerts cannot see), reads the\n" +
			"ObjectStore it names and requires its destination to be this deployment's cnpg bucket, waits for\n" +
			"continuous WAL archiving, takes an on-demand Backup and requires it completed, then reads the\n" +
			"bucket through a temporary read-only key and requires the base backup and its end WAL segment\n" +
			"under the exact destinationPath and serverName the live objects name — and, where objProxy is on,\n" +
			"that a keyless read is refused (SSE-C). Finally requires Prometheus to have loaded the\n" +
			"llz-cnpg-backup rule group. bootstrap-openbao runs this after converge.\n" +
			"\n" +
			"--drill adds the round trip, and is refused on any --region but the e2e lane's own deployments\n" +
			"(e2e, e2e-<forge>): creates a canary database (a CNPG Database) in the live cluster BEFORE the\n" +
			"backup, then bootstraps a scratch Cluster in its own namespace by barman recovery from that backup\n" +
			"and fails unless the canary database is in it. The scratch namespace and the canary are removed\n" +
			"afterwards; the drill never writes to the live archive.\n" +
			"\n" +
			"Needs LINODE_API_TOKEN. Skips cleanly when spec.components.cnpgBackup is disabled for --region.\n" +
			"See docs/runbooks/cnpg-backup-restore.md.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return Run(context.Background(), region, drill)
		},
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) to check (required)")
	c.Flags().BoolVar(&drill, "drill", false, "also restore the backup into a scratch cluster and read a canary back (e2e lane deployments only)")
	return c
}
