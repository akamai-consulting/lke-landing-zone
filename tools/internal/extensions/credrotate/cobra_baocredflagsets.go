package credrotate

// cobra_baocredflagsets.go — the flag sets for the three credential verbs that
// needed the OpenBao write seams.
//
// The verbs are tools/internal/credrotate; the write path they reach —
// baoread.KVPut and baoread.ExecStdin — is installed by baoread_deps.go.

import (
	"github.com/spf13/cobra"
)

func RotateInclusterPATCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate-incluster-pat",
		Short: "mint a fresh narrow in-cluster PAT, write it to this region's OpenBao, drain old siblings",
		Long: "Replaces `llz ci propagate-pat`: instead of round-tripping the BROAD PAT\n" +
			"through a GitHub secret into every cluster, each region's job mints its own\n" +
			"NARROW in-cluster PAT (label llz-incluster-<objLabelPrefix>-<region>) with the broad token,\n" +
			"verifies it, writes secret/linode/api-token via the secret-propagator\n" +
			"GitHub-OIDC role (payload on stdin, never argv), and drains older same-labeled\n" +
			"siblings past the grace window. The token never crosses a job boundary and\n" +
			"never touches a GitHub secret. Regions without an OpenBao pod skip with a\n" +
			"summary note. Needs `permissions: id-token: write`. Env: REGION,\n" +
			"LINODE_API_TOKEN (broad, mints), GITHUB_REPOSITORY,\n" +
			"ACTIONS_ID_TOKEN_REQUEST_{URL,TOKEN}.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunRotateInClusterPAT() },
	}
}

func MintBootstrapPATCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "mint-bootstrap-pat",
		Short: "mint the first narrow in-cluster Linode PAT and seed secret/linode/api-token",
		Long: "Bootstrap-time twin of rotate-incluster-pat: mints the narrow in-cluster PAT\n" +
			"(domains/object_storage/volumes rw + linodes/vpc ro + firewall rw) with the\n" +
			"broad provisioning PAT and seeds secret/linode/api-token — the single rotating\n" +
			"token every in-cluster Linode consumer reads. Idempotent: an already-seeded\n" +
			"path is skipped, so a re-bootstrap never clobbers a rotation-minted token.\n" +
			"Reads LINODE_API_TOKEN (mint), OPENBAO_ROOT_TOKEN (seed).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunMintBootstrapPAT(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment whose in-cluster PAT to mint (required)")
	return c
}

func MintBootstrapObjkeysCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "mint-bootstrap-objkeys",
		Short: "mint the first Loki/Harbor/platform-obj object-storage keys and seed them into OpenBao",
		Long: "Bootstrap-time twin of the in-cluster rotator: mints the region's scoped\n" +
			"object-storage keys (Loki chunks/ruler/admin, Harbor registry, and the broad\n" +
			"managed platform-obj key spanning all buckets) via the Linode API and seeds\n" +
			"secret/loki/object-store + secret/harbor/registry-s3 + secret/obj/platform in one\n" +
			"step — no Terraform-minted keys, no LOKI_S3_*/HARBOR_REGISTRY_S3_* GitHub\n" +
			"secrets, no stash/reseed relay. Idempotent: already-seeded paths are\n" +
			"skipped (a rotator-minted key is never clobbered). Seeds carry rotated_at\n" +
			"so the rotator adopts them on its own cadence. Reads LINODE_API_TOKEN,\n" +
			"OPENBAO_ROOT_TOKEN; obj_cluster from the object-storage tfvars.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunMintBootstrapObjkeys(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment whose keys to mint (required)")
	return c
}
