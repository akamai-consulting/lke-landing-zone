package teardown

import (
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/spf13/cobra"
)

// cobra_drain_obj_buckets.go — the flag set for `llz ci drain-obj-buckets`.
//
// It came back out for the ordinary reason and one specific one: the command is
// DESTRUCTIVE and gated on the GLOBAL --yes, which is package main's confirm
// plumbing. A lane that deletes log chunks and registry blobs should read that
// gate where the gate lives, not carry a copy of it.

func DrainObjBucketsCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "drain-obj-buckets",
		Short: "delete every object in the deployment's Loki/Harbor buckets, leaving the buckets in place",
		Long: "Empties the data buckets a deployment owns. DESTRUCTIVE: it deletes log chunks\n" +
			"and registry blobs. Requires --yes.\n\n" +
			"For a lane that recreates a cluster over the same buckets. Each cluster mints its\n" +
			"own SSE-C key, so a previous cluster's objects are permanently unreadable and make\n" +
			"Loki's index-gateway fail whole tables rather than skip them.\n\n" +
			"EMPTIES rather than deletes: Linode does not free a bucket NAME promptly after\n" +
			"deletion, so destroying the buckets fails the next provision with\n" +
			"\"already exists\".",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return RunDrainObjBuckets(region, cliopts.Global.Yes)
		},
	}
	c.Flags().StringVar(&region, "region", os.Getenv("REGION"), "deployment whose buckets to empty")
	return c
}

// OpenBaoBackupDestroyGuardCmd is `llz ci openbao-backup-destroy-guard`.
func OpenBaoBackupDestroyGuardCmd() *cobra.Command {
	var region string
	var allow bool
	c := &cobra.Command{
		Use:   "openbao-backup-destroy-guard",
		Short: "before an object-storage destroy: refuse while a backup bucket (OpenBao snapshots, Velero backups) holds data, unless --allow-delete",
		Long: "Run by the object-storage destroy job before anything is drained. The deployment's\n" +
			"openbao-backup and velero buckets exist to outlive the cluster, so a destroy may not\n" +
			"delete either while it holds any object version (noncurrent ones included) unless the\n" +
			"caller opts in with --allow-delete (the workflow's drain_data_buckets). Every bucket is\n" +
			"judged before any is emptied. With the opt-in it deletes every version and delete marker\n" +
			"and re-lists to prove each bucket is empty. Fails closed on anything it cannot read.\n" +
			"--allow-delete also needs --yes. Needs LINODE_API_TOKEN; run from the instance repo root.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return RunOpenBaoBackupGuard(region, allow, cliopts.Global.Yes)
		},
	}
	c.Flags().StringVar(&region, "region", os.Getenv("REGION"), "deployment whose backup buckets to guard")
	c.Flags().BoolVar(&allow, "allow-delete", false, "consent to deleting the OpenBao and Velero backups with their buckets (drain_data_buckets)")
	return c
}
