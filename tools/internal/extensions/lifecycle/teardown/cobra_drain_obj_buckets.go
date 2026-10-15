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
		Short: "before an object-storage destroy: refuse while the OpenBao backup bucket holds a snapshot, unless --allow-delete",
		Long: "Run by the object-storage destroy job before anything is drained. The deployment's\n" +
			"openbao-backup bucket exists to outlive the cluster, so a destroy may not delete it\n" +
			"while it holds any object version (noncurrent ones included) unless the caller opts in\n" +
			"with --allow-delete (the workflow's drain_data_buckets). With the opt-in it deletes every\n" +
			"version and delete marker and re-lists to prove the bucket is empty. Fails closed on\n" +
			"anything it cannot read. --allow-delete also needs --yes. Needs LINODE_API_TOKEN; run\n" +
			"from the instance repo root.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return RunOpenBaoBackupGuard(region, allow, cliopts.Global.Yes)
		},
	}
	c.Flags().StringVar(&region, "region", os.Getenv("REGION"), "deployment whose backup bucket to guard")
	c.Flags().BoolVar(&allow, "allow-delete", false, "consent to deleting the OpenBao backups with the bucket (drain_data_buckets)")
	return c
}
