package main

// ci_credflagsets.go — the `llz ci rotate-broad-pat` and `temp-objkey` flag sets.
//
// Both verbs are tools/internal/credrotate, which owns the credential-rotation
// family end to end after three wall extractions.

import (
	"context"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/credrotate"
	"github.com/spf13/cobra"
)

func ciTempObjkeyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "temp-objkey",
		Short: "mint/delete a short-lived scoped OBJ key for the destroy-time bucket drain",
	}

	var region, endpoint, buckets string
	create := &cobra.Command{
		Use:   "create",
		Short: "mint a temporary read_write key on the given buckets; export TEMP_OBJKEY_* via $GITHUB_ENV",
		Long: "Mints a short-lived scoped key (label llz-drain-<region> — distinct from\n" +
			"the rotator's labels so its keep-newest-N drain never touches it) with\n" +
			"read_write on --buckets, for the destroy job's s5cmd sweep. The OBJ\n" +
			"cluster is derived from --endpoint (https://<cluster>.linodeobjects.com —\n" +
			"the module's s3_endpoint output). Exports TEMP_OBJKEY_ID/ACCESS/SECRET to\n" +
			"$GITHUB_ENV (secret masked). Pair with `temp-objkey delete` in an\n" +
			"always() step. Reads LINODE_API_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return credrotate.RunTempObjkeyCreate(region, endpoint, buckets)
		},
	}
	create.Flags().StringVar(&region, "region", "", "deployment name — labels the key llz-drain-<region> (required)")
	create.Flags().StringVar(&endpoint, "endpoint", "", "S3 endpoint URL, e.g. https://us-ord-1.linodeobjects.com (required)")
	create.Flags().StringVar(&buckets, "buckets", "", "comma-separated bucket labels the key may drain (required)")

	del := &cobra.Command{
		Use:   "delete",
		Short: "revoke the temporary key exported by `temp-objkey create` (no-op when unset)",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return credrotate.RunTempObjkeyDelete() },
	}

	c.AddCommand(create, del)
	return c
}

func ciRotateBroadPATCmd() *cobra.Command {
	var apply bool
	c := &cobra.Command{
		Use:   "rotate-broad-pat",
		Short: "rotate the broad Linode CI/TF PAT in-cluster: mint, seed OpenBao, publish to GitHub env secrets, revoke old",
		Long: "In-cluster rotator for the broad account:read_write Linode PAT (LINODE_API_TOKEN).\n" +
			"Runs in a dedicated CronJob (not the reconciler). When the OpenBao rotated_at is\n" +
			"older than --rotate-after-days: mints a fresh broad PAT with the current token,\n" +
			"verifies it, publishes it to each infra-<deployment>, writes it to OpenBao GitHub\n" +
			"environment secret (sealed box), then revokes older same-labeled PATs outside the\n" +
			"grace window. Dry-run unless --apply. Env: LINODE_TOKEN (current broad PAT),\n" +
			"BROAD_PAT_LABEL, BROAD_PAT_DEPLOYMENTS (space-separated), GH_TOKEN, GH_REPO,\n" +
			"ROTATE_AFTER_DAYS, GRACE_DAYS, OPENBAO_* (Kubernetes-auth: broad-pat-rotator role).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return credrotate.RunRotateBroadPAT(context.Background(), apply)
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "actually rotate; without it, report whether a rotation is due and exit")
	return c
}
