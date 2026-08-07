package main

// ci_teardown.go — the destroy verbs, reduced to flag sets.
//
// Everything they do is `teardown` and lives in tools/internal/teardown. What is
// left here is teardownDeps(): the seven capabilities that package is HANDED.
//
// SIX OF THE SEVEN ARE THE SAME FOUR the earlier extractions named — a cloud
// credential, a cluster client, a shell-out, a summary sink — plus the two shapes
// a destroy needs that an in-cluster lane does not: a kubeconfig it resolves at
// run time, and the terraform binary.
//
// THE SEVENTH IS `Confirm`, AND IT IS NOT A CAPABILITY. It is an AUTHORISATION —
// `--yes`, the answer to "may I", not the means to act. The grant vocabulary has
// no way to say it: `cloud-mutate` declares that this binding may delete cloud
// resources and says nothing about whether a human agreed to THIS deletion. That
// distinction never came up in the first five extensions because none of them
// destroys anything, and it is a live question for the action ABI.

import (
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/teardown"
	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tfvars"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tfbin"
)

func teardownDeps() teardown.Deps {
	return teardown.Deps{
		Client:         linode.ClientFromEnv,
		Token:          linode.TokenFromEnv,
		Exec:           execOutput,
		TempKubeconfig: cigate.WriteTempKubeconfig,
		RegionTFVars:   func(dir, region string) (tf.TFVars, string, error) { return tfvars.ReadRegion(dir, region) },
		Combined: func(kubeconfigPath string, args ...string) (string, bool) {
			cmd := exec.Command("kubectl", args...)
			cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
			return cigate.RunCombined(cmd)
		},
		Summary: ghaout.Append,
		TFBin:   tfbin.Bin,
		Confirm: func() bool { return gopts.yes },
	}
}

func ciTeardownCaptureCmd() *cobra.Command {
	var region, tfDir string
	c := &cobra.Command{
		Use:   "teardown-capture",
		Short: "snapshot the cluster id + its attached pvc-* Volume ids before a destroy",
		Long: "Native port of the destroy job's 'Capture LKE cluster id + pvc Volume ids'\n" +
			"step. Both post-destroy sweeps must be scoped to THIS cluster, never the\n" +
			"region — co-located deployments share a Linode region and the block-storage\n" +
			"tag, so a region filter would never converge and could delete a live peer's\n" +
			"resources. NodeBalancers scope by the cluster's CCM tag (just the id), but\n" +
			"Volumes carry no cluster id and lose their linode_id once detached — so the\n" +
			"pvc-* Volumes attached to this cluster's nodes are snapshotted, by id, while\n" +
			"the cluster still exists. Writes LKE_CLUSTER_ID and CLUSTER_PVC_VOLUME_IDS\n" +
			"to $GITHUB_ENV. Read-only; reads LINODE_TOKEN (or LINODE_API_TOKEN).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return teardown.RunCapture(teardownDeps(), region, tfDir) },
	}
	c.Flags().StringVar(&region, "region", "", "tfvars prefix, e.g. primary (required)")
	c.Flags().StringVar(&tfDir, "tf-dir", teardown.DefaultClusterTFDir, "cluster terraform root holding <region>.tfvars")
	return c
}

func ciTeardownForceDeleteCmd() *cobra.Command {
	var region, tfDir string
	c := &cobra.Command{
		Use:   "teardown-force-delete",
		Short: "force-delete the LKE cluster + node firewall stragglers after a destroy (--yes to delete)",
		Long: "Native port of the destroy job's 'Force-delete remaining Linode resources'\n" +
			"step: deletes the cluster by label if terraform couldn't (indeterminate\n" +
			"state / failed import), then the node-pool firewall — resolved by the exact\n" +
			"node_firewall_id terraform output first, then by the module-correct label\n" +
			"(firewall_label tfvars var, NOT a reconstructed \"<cluster>-nodes\" guess:\n" +
			"hardcoding that ignored var.firewall_label and leaked the firewall every\n" +
			"teardown). Delete failures warn rather than fail — this is always()-path\n" +
			"selfupgrade.Cleanup and the orphan reaper backstops it. Dry-run by default; --yes to\n" +
			"delete. Reads LINODE_TOKEN (or LINODE_API_TOKEN).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return teardown.RunForceDelete(teardownDeps(), region, tfDir)
		},
	}
	c.Flags().StringVar(&region, "region", "", "tfvars prefix, e.g. primary (required)")
	c.Flags().StringVar(&tfDir, "tf-dir", teardown.DefaultClusterTFDir, "cluster terraform root holding <region>.tfvars")
	return c
}

func ciTeardownDeleteVPCCmd() *cobra.Command {
	var region, tfDir, clusterID string
	var attempts, retryDelay int
	var requireDeleted bool
	c := &cobra.Command{
		Use:   "teardown-delete-vpc",
		Short: "delete the cluster VPC, retrying the post-destroy in-use window (--yes to delete)",
		Long: "Native port of the destroy job's 'Delete cluster VPC' step. A VPC deletes\n" +
			"only once NOTHING is parked in its subnet — CCM NodeBalancers and node\n" +
			"Linodes both block it with a 409 — so this must run LAST, after the Volume\n" +
			"and NodeBalancer sweeps, and retries to ride out the async-release window.\n" +
			"Resolves the exact vpc_id terraform output first, then the\n" +
			"\"<cluster_label>-vpc\" label. Still-undeletable after the retries is a\n" +
			"warning by default; --require-deleted turns it into a NON-ZERO exit so a\n" +
			"destroy doesn't go color.Green leaving an orphan VPC that blocks the next apply's\n" +
			"preflight. Dry-run by default; --yes to delete. Reads LINODE_TOKEN (or\n" +
			"LINODE_API_TOKEN).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return teardown.RunDeleteVPC(teardownDeps(), region, tfDir, clusterID, attempts, retryDelay, requireDeleted)
		},
	}
	c.Flags().StringVar(&region, "region", "", "tfvars prefix, e.g. primary (required)")
	c.Flags().StringVar(&tfDir, "tf-dir", teardown.DefaultClusterTFDir, "cluster terraform root holding <region>.tfvars")
	c.Flags().StringVar(&clusterID, "cluster-id", "", "LKE cluster id — resolves the LKE-E auto VPC labeled lke<id> (defaults to the LKE_CLUSTER_ID env teardown-capture exports, so the workflow needs no new flag)")
	c.Flags().IntVar(&attempts, "attempts", 6, "delete attempts before giving up")
	c.Flags().IntVar(&retryDelay, "retry-delay", 20, "seconds between delete attempts")
	c.Flags().BoolVar(&requireDeleted, "require-deleted", false, "fail (non-zero) if the VPC is still undeletable after all attempts")
	return c
}

func ciAssertNoOrphansCmd() *cobra.Command {
	var region, volumeRegion, clusterID, env string
	var threshold, attempts, retryDelay int
	c := &cobra.Command{
		Use:   "assert-no-orphans",
		Short: "fail if orphaned Linode resources remain after a destroy (with retry)",
		Long: "Final destroy-job gate. Counts orphaned Volumes / NodeBalancers / VPCs with\n" +
			"the SAME account census `llz ci preflight` runs, and FAILS the job when the\n" +
			"count EXCEEDS --threshold — so a destroy can't go color.Green leaving orphans that\n" +
			"stall the next apply's preflight. This backstops the scoped Volume/NB sweeps,\n" +
			"which no-op when the cluster was already gone at capture time (a re-run of a\n" +
			"partial destroy, or pre-existing orphans). Cluster-delete reaps NBs/VPCs\n" +
			"asynchronously and Volumes detach as the nodes tear down, so --attempts\n" +
			"re-counts (sleeping --retry-delay s) to ride out that settling window before\n" +
			"failing. Read-only — deletes NOTHING; clear survivors with `llz reap`.\n" +
			"NB/VPC are counted account-wide (cluster-id attributable); --volume-region\n" +
			"scopes the pvc-* Volume count (detached Volumes carry no cluster id, so an\n" +
			"account-wide count would flag other regions'/teams' Volumes reap can't clean).\n" +
			"--region scopes the NB/VPC census (empty = account-wide, matching preflight).\n" +
			"Reads LINODE_TOKEN (or LINODE_API_TOKEN).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return teardown.RunAssertNoOrphans(teardownDeps(), region, volumeRegion, clusterID, env, threshold, attempts, retryDelay)
		},
	}
	f := c.Flags()
	f.StringVar(&region, "region", "", "scope the NB/VPC orphan census to one region (empty = account-wide)")
	f.StringVar(&volumeRegion, "volume-region", "", "scope the pvc-* Volume orphan count to one region (empty = the --region value, or account-wide)")
	f.StringVar(&env, "env", "", "deployment name; widens the Volume census to that deployment's RELABELED Volumes. Without it this gate counts only `pvc-`-prefixed Volumes and reports zero orphans while the deployment's own renamed Volumes leak.")
	f.StringVar(&clusterID, "cluster-id", "", "the destroyed cluster's id: its OWN surviving NBs (lke_cluster.id) and VPC (lke<id>) fail the gate regardless of --threshold (defaults to the LKE_CLUSTER_ID env, so the workflow needs no new flag)")
	f.IntVar(&threshold, "threshold", 0, "only fail when the (other-account) orphan count EXCEEDS this")
	f.IntVar(&attempts, "attempts", 5, "re-count attempts before failing")
	f.IntVar(&retryDelay, "retry-delay", 30, "seconds between re-count attempts")
	return c
}

func ciDestroyUnwedgeCmd() *cobra.Command {
	var region string
	cmd := &cobra.Command{
		Use:   "destroy-unwedge",
		Short: "clear Argo/discovery/CNPG finalizer deadlocks before helm uninstalls apl (destroy-time)",
		Long: "Native port of null_resource.unwedge_namespace_finalizers_on_destroy's\n" +
			"local-exec heredoc, now a standalone CI destroy step. Runs while the cluster is\n" +
			"still up and clears the wedges that otherwise make `helm uninstall apl`'s\n" +
			"--wait time out: scales down Argo CD, strips resources-finalizer.argocd from\n" +
			"Applications/AppProjects, deletes stale aggregated APIServices (dead backing\n" +
			"Service → Available=False → discovery failure), and strips CNPG cluster/pooler\n" +
			"finalizers. All best-effort and non-fatal (always exit 0); a subsequent\n" +
			"cluster delete reaps everything regardless.\n\n" +
			"Kubeconfig source, in order: $KUBECONFIG_B64 (base64), then $KUBECONFIG (a\n" +
			"file), then --region — which resolves the cluster by its <region>.tfvars\n" +
			"label via the Linode API (LINODE_TOKEN / LINODE_API_TOKEN). When --region\n" +
			"finds no live cluster (already reaped), this is a clean no-op.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return teardown.RunDestroyUnwedge(teardownDeps(), region) },
	}
	cmd.Flags().StringVar(&region, "region", "", "tfvars prefix (e.g. primary); resolve the cluster kubeconfig by label via the Linode API when KUBECONFIG_B64/KUBECONFIG are unset")
	return cmd
}
