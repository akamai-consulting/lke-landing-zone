package clusteraccess

// cobra_cluster_access.go — the kubeconfig and runner-ACL verbs, reduced to flag sets.
//
// Everything they do is `cluster-access` and lives in tools/internal/extensions/lifecycle/clusteraccess.
// One seam stays here: the shell-out. The terraform binary behind the state read
// now comes from internal/tfbin, which became a package on this extraction because
// eight things wanted it.

import (
	"github.com/spf13/cobra"
)

func ClusterAccessDeps() Deps {
	return Deps{Exec: execOutput}
}

func RunnerACLCmd() *cobra.Command {
	var o ACLOpts
	c := &cobra.Command{
		Use:   "runner-acl <open|revoke>",
		Short: "add/remove this runner's egress IP in the LKE-E control-plane ACL",
		Long: "Native port of the lke-runner-acl composite action. open detects this\n" +
			"runner's public egress IP and adds it to the cluster's control-plane ACL so\n" +
			"kubectl is permitted; revoke removes it again (run with `if: always()`). open\n" +
			"records what it changed in a per-region state file under RUNNER_TEMP so revoke\n" +
			"is self-describing and idempotent — a no-op when open made no change (ACL\n" +
			"disabled, or the IP was already present). Reads LINODE_API_TOKEN (or\n" +
			"LINODE_TOKEN); an empty token no-ops with a warning so the failure surfaces\n" +
			"later on kubectl with a clear ACL message. The cluster is resolved from\n" +
			"--cluster-id, else --cluster-label (+ --linode-region), else cluster_label /\n" +
			"region read from <tfvars-dir>/<region>.tfvars.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// cmd.Flags() includes flags inherited from the root, so this picks up
			// the global --dry-run. It was previously never read: the flag parsed
			// fine, printed nothing, and the command mutated the ACL anyway.
			o.DryRun, _ = cmd.Flags().GetBool("dry-run")
			return RunACL(ClusterAccessDeps(), args[0], o)
		},
	}
	f := c.Flags()
	f.StringVar(&o.Region, "region", "", "deployment/env key (names the state file; finds <region>.tfvars)")
	f.StringVar(&o.ClusterID, "cluster-id", "", "explicit LKE cluster numeric ID (skips label resolution)")
	f.StringVar(&o.ClusterLabel, "cluster-label", "", "LKE cluster label to resolve by")
	f.StringVar(&o.LinodeRegion, "linode-region", "", "Linode datacenter region (e.g. us-ord) to disambiguate")
	f.StringVar(&o.Ip, "ip", "", "egress IP to add/remove (default: auto-detect)")
	f.StringVar(&o.TfvarsDir, "tfvars-dir", "terraform-iac-bootstrap/cluster", "dir holding <region>.tfvars")
	f.BoolVar(&o.FailOnMissing, "fail-on-missing", true, "open: fail if the cluster can't be resolved")
	f.BoolVar(&o.ConfigMap, "runner-configmap", false, "also lease/release the IP in the firewall-runner-acl ConfigMap so the internal-CIDR firewall controller preserves it (requires KUBECONFIG)")
	return c
}

func FetchKubeconfigCmd() *cobra.Command {
	var o FetchOpts
	c := &cobra.Command{
		Use:   "fetch-kubeconfig",
		Short: "write an LKE cluster's kubeconfig (from the Linode API) to a file",
		Long: "Fetch the cluster's admin kubeconfig directly from the Linode API and write\n" +
			"it to --output (mode 0600). The API-sourced alternative to reading\n" +
			"kubeconfig_raw out of Terraform state — no terraform init / S3 backend / git\n" +
			"auth, and no empty-output failure class. The cluster is resolved from\n" +
			"--cluster-id, else --cluster-label (+ --linode-region), else cluster_label /\n" +
			"region in <tfvars-dir>/<region>.tfvars. Reads LINODE_API_TOKEN (or\n" +
			"LINODE_TOKEN). With --allow-missing, an absent/not-ready kubeconfig sets\n" +
			"available=false on GITHUB_OUTPUT instead of failing.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return RunFetch(o) },
	}
	f := c.Flags()
	f.StringVar(&o.Output, "output", "", "absolute path to write the kubeconfig to (required)")
	f.StringVar(&o.Ref.Region, "region", "", "deployment/env key (finds <region>.tfvars)")
	f.StringVar(&o.Ref.ClusterID, "cluster-id", "", "explicit LKE cluster numeric ID (skips label resolution)")
	f.StringVar(&o.Ref.ClusterLabel, "cluster-label", "", "LKE cluster label to resolve by")
	f.StringVar(&o.Ref.LinodeRegion, "linode-region", "", "Linode datacenter region (e.g. us-ord) to disambiguate")
	f.StringVar(&o.Ref.TfvarsDir, "tfvars-dir", "terraform-iac-bootstrap/cluster", "dir holding <region>.tfvars")
	f.BoolVar(&o.AllowMissing, "allow-missing", false, "set available=false instead of failing when the kubeconfig is absent")
	return c
}

func FetchKubeconfigStateCmd() *cobra.Command {
	var region, output string
	var allowMissing bool
	c := &cobra.Command{
		Use:   "fetch-kubeconfig-state",
		Short: "init the cluster TF backend and write the kubeconfig_raw output to a file",
		Long: "Native port of the fetch-kubeconfig composite action's inline body. Runs\n" +
			"`terraform init` against the cluster/<region> state key (bucket from\n" +
			"$TF_STATE_BUCKET; init output is NOT swallowed — a failed init is the top\n" +
			"suspect when `terraform output` reads empty against a state that has the\n" +
			"value), then writes `terraform output -raw kubeconfig_raw` to --output\n" +
			"(mode 0600). An empty output dumps grouped diagnostics (output keys, state\n" +
			"resources) and either fails or, with --allow-missing, sets available=false\n" +
			"on GITHUB_OUTPUT. Run from the cluster terraform working directory.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunFetchFromState(ClusterAccessDeps(), region, output, allowMissing)
		},
	}
	c.Flags().StringVar(&region, "region", "", "deployment/env key, e.g. primary (required)")
	c.Flags().StringVar(&output, "output", "", "absolute path to write the kubeconfig to (required)")
	c.Flags().BoolVar(&allowMissing, "allow-missing", false, "set available=false instead of failing when kubeconfig_raw is absent")
	return c
}
