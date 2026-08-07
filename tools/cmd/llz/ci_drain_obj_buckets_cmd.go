package main

import (
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/teardown"
	"github.com/spf13/cobra"
)

// ci_drain_obj_buckets_cmd.go — the flag set for `llz ci drain-obj-buckets`.
//
// It came back out for the ordinary reason and one specific one: the command is
// DESTRUCTIVE and gated on the GLOBAL --yes, which is package main's confirm
// plumbing. A lane that deletes log chunks and registry blobs should read that
// gate where the gate lives, not carry a copy of it.

func ciDrainObjBucketsCmd() *cobra.Command {
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
			return teardown.RunDrainObjBuckets(region, cliopts.Global.Yes)
		},
	}
	c.Flags().StringVar(&region, "region", os.Getenv("REGION"), "deployment whose buckets to empty")
	return c
}
