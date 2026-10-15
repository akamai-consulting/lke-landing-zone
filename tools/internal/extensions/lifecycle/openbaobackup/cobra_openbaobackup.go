package openbaobackup

// cobra_openbaobackup.go — the CLI surface. Flag wiring and help text only.

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
)

// BackupCmd is `llz ci openbao-backup`, the openbaoBackup CronJob's verb.
func BackupCmd() *cobra.Command {
	var keep int
	c := &cobra.Command{
		Use:   "openbao-backup",
		Short: "in-cluster: snapshot OpenBao's Raft store to the backup bucket, verify it, apply retention",
		Long: "Runs as the openbaoBackup CronJob (platform-apl/components/openbaoBackup). Logs in\n" +
			"with the openbao-backup Kubernetes-auth role, reads the backup bucket's key from\n" +
			"secret/obj/openbao-backup, takes a Raft snapshot from the active node, verifies the\n" +
			"archive, uploads it, reads it back, then deletes all but the newest --keep snapshots.\n" +
			"Prints one JSON audit record (event=openbao-backup) as its last line.\n" +
			"\n" +
			"Env: OPENBAO_ADDR (default the chart's -active Service), OPENBAO_TLS_SERVER_NAME,\n" +
			"OPENBAO_KUBERNETES_ROLE (default openbao-backup), OPENBAO_CA_FILE,\n" +
			"OPENBAO_CLIENT_CERT_FILE, OPENBAO_CLIENT_KEY_FILE, KEEP.\n" +
			"See docs/runbooks/openbao-backup-restore.md.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if keep <= 0 {
				keep = int(cli.EnvInt("KEEP", DefaultKeep))
			}
			rec, err := RunBackup(context.Background(), keep)
			if perr := printRecord(rec); perr != nil && err == nil {
				err = perr
			}
			if err != nil {
				return fmt.Errorf("openbao-backup: %w", err)
			}
			return nil
		},
	}
	c.Flags().IntVar(&keep, "keep", 0, fmt.Sprintf("snapshots to retain, including this run's (default $KEEP or %d)", DefaultKeep))
	return c
}

// DrillCmd is `llz ci assert-openbao-restore`, the e2e restore drill.
func DrillCmd() *cobra.Command {
	var region string
	var backupOnly bool
	c := &cobra.Command{
		Use:   "assert-openbao-restore",
		Short: "e2e: write a canary, run the backup Job, restore its snapshot, and require the canary back",
		Long: "The gate for OpenBao backup AND restore. Writes a canary to OpenBao, runs a Job built\n" +
			"from the openbaoBackup CronJob, fetches the exact snapshot it uploaded through a\n" +
			"temporary read-only bucket key, overwrites the canary, restores the snapshot through the\n" +
			"same functions `llz openbao restore` uses, and fails unless the canary reads back its\n" +
			"snapshotted value.\n" +
			"\n" +
			"RESTORES THE LIVE STORE IN PLACE, so it refuses any --region but the e2e lane's own\n" +
			"deployments (e2e, e2e-<forge>). Needs OPENBAO_ROOT_TOKEN (so it runs before the bootstrap\n" +
			"job revokes root) and LINODE_API_TOKEN. Re-writes any tracked credential the rollback\n" +
			"reverted.\n" +
			"\n" +
			"--backup-only runs the producer Job and the consumer-side fetch + verify and stops: no\n" +
			"canary, no root, no restore, any deployment. bootstrap-openbao runs it everywhere so a\n" +
			"backup that cannot work fails the bootstrap instead of the first scheduled tick.\n" +
			"\n" +
			"Skips cleanly when spec.components.openbaoBackup is disabled for --region.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if backupOnly {
				return RunBackupCheck(context.Background(), region)
			}
			return RunDrill(context.Background(), region)
		},
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) to drill (required)")
	c.Flags().BoolVar(&backupOnly, "backup-only", false, "only run the backup Job and verify its upload through the restore path; never restores (safe on any deployment)")
	return c
}
