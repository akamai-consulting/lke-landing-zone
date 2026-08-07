package bootstrapcluster

import (
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/answers"
	"github.com/spf13/cobra"
)

// The one edge internal/bootstrapcluster could not bring with it: the pinned
// template ref reads the copier answers file, which lives in the blocked scaffold
// mass. Installed here as a delegating closure, never by assignment — a direct
// assignment would snapshot pinnedTemplateRef before any test could swap it.
func Init() {
	PinnedTemplateRef = func() string { return answers.PinnedTemplateRef() }
}

// cobra_bootstrap_cluster.go — the flag sets for `llz ci bootstrap-cluster` and
// `llz ci prepare-apl-upgrade`. The lanes are internal/bootstrapcluster.
//
// BootstrapFlags is EXPORTED and threaded as a parameter, unlike
// internal/reconciler's 25-field opts which stayed with its package. Eight fields,
// one caller, and every one of them is a genuine command-line input a human or a
// workflow supplies — that is a parameter, not a configuration surface.

func BootstrapClusterCmd() *cobra.Command {
	var f BootstrapFlags
	c := &cobra.Command{
		Use:   "bootstrap-cluster",
		Short: "layer LLZ's Argo bridge onto a Linode MANAGED App Platform cluster (apl_enabled)",
		Long: "Layers LLZ's extras onto a Linode-managed apl-core (apl_enabled=true).\n" +
			"Linode owns the apl-core install + the lke<id>.akamai-apl.net domain/DNS/cert,\n" +
			"so this command does NOT install apl-core. It waits for the managed ArgoCD to\n" +
			"be able to admit Applications, then server-side-applies the Argo bridge (the\n" +
			"platform-bootstrap AppProject + Application + the llz-secret-store Application),\n" +
			"the named non-default block-storage-retain StorageClass the OpenBao PVC\n" +
			"references, and the llz-openbao namespace. Idempotent (server-side apply), so CI\n" +
			"re-runs it every apply. Reads the kubeconfig from --kubeconfig (or\n" +
			"KUBECONFIG_RAW), and the optional private-fork GHCR creds from\n" +
			"GHCR_USERNAME / GHCR_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunBootstrapCluster(f) },
	}
	fl := c.Flags()
	fl.StringVar(&f.Kubeconfig, "kubeconfig", "", "path to the cluster kubeconfig (from the fetch-kubeconfig action); falls back to $KUBECONFIG_RAW")
	fl.StringVar(&f.Env, "env", "", "apl-values environment subdir, e.g. primary (required)")
	fl.StringVar(&f.ClusterID, "cluster-id", "", "numeric LKE cluster id (the `cluster` workspace's cluster_id output); rendered into block-storage-retain's lke<id> volumeTag. Falls back to $LKE_CLUSTER_ID. Required — a StorageClass without it provisions un-reapable Volumes")
	fl.StringVar(&f.AppsRepoRevision, "apps-repo-revision", "", "bootstrap Application targetRevision (default: spec, then main)")
	fl.StringVar(&f.InstanceRepo, "instance-repo", "", "owner/name of the instance repo (bootstrap App source) (required)")
	fl.StringVar(&f.UpstreamOrg, "upstream-org", "akamai-consulting", "template repo org (llz-secret-store App source + AppProject sourceRepos)")
	fl.StringVar(&f.TemplateRef, "template-ref", "", "template repo ref for the llz-secret-store Application (default: the instance's pin, then main)")
	fl.BoolVar(&f.DryRun, "dry-run", false, "print the intended actions without touching the cluster")
	return c
}

func PrepareAplUpgradeCmd() *cobra.Command {
	var kubeconfig string
	c := &cobra.Command{
		Use:   "prepare-apl-upgrade",
		Short: "annotate apl-operator so the apl-core 6.0.x → 6.1.x upgrade can sync",
		Long: "Applies apl-core 6.1.0's documented pre-upgrade prerequisite: the\n" +
			AplSyncOptionsKey + "=" + AplSyncOptionsValue + " annotation on the\n" +
			AplOperatorNamespace + "/" + AplOperatorDeployment + " Deployment.\n\n" +
			"6.1.0 ships this annotation in its own chart, but that value only exists\n" +
			"AFTER the upgrade has synced — and the sync is what needs it. Annotating\n" +
			"beforehand turns that sync into a Replace instead of a failing apply.\n\n" +
			"On the managed App Platform Linode owns when the upgrade rolls, so run this\n" +
			"ahead of time (bootstrap-cluster also runs it on every apply). Idempotent;\n" +
			"a no-op once the cluster is on 6.1.x.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path, cleanup, err := ResolveKubeconfig(kubeconfig)
			if err != nil {
				return err
			}
			defer cleanup()
			d := NewBootstrapDeps(path)
			applied, err := PrepareAplUpgrade(d)
			if err != nil {
				return err
			}
			if !applied {
				fmt.Printf("no %s/%s Deployment on this cluster — nothing to prepare.\n",
					AplOperatorNamespace, AplOperatorDeployment)
				return nil
			}
			fmt.Printf("%s/%s annotated %s=%s — the apl-core 6.1.x upgrade can now sync.\n",
				AplOperatorNamespace, AplOperatorDeployment, AplSyncOptionsKey, AplSyncOptionsValue)
			return nil
		},
	}
	c.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to the kubeconfig (default: KUBECONFIG_RAW / ambient)")
	return c
}
