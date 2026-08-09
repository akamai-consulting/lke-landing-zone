package firewall

// cobra_firewall.go — the CLI surface for firewall.
//
// Split from firewall.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func Cmd() *cobra.Command {
	var region string
	cmd := &cobra.Command{
		Use:   "bootstrap-cloud-firewall",
		Short: "seed the firewall-controller's token Secret + config ConfigMap on the cluster",
		Long: "Native port of bootstrap-cloud-firewall.sh. Seeds the in-cluster state the\n" +
			"custom firewall-controller (tools/firewall-controller) needs to reconcile the\n" +
			"node-pool Linode Cloud Firewall in place: the kube-system/linode API-token\n" +
			"Secret (key=token) and the linode-internal-cidr-firewall-config ConfigMap\n" +
			"with LINODE_FIREWALL_ID (plus LKE_CLUSTER_ID when CLUSTER_ID is set, enabling\n" +
			"control-plane ACL reconciliation). Idempotent — safe to re-run.\n\n" +
			"--region <prefix> resolves LINODE_FIREWALL_ID, CLUSTER_ID and VPC_CIDR from\n" +
			"<prefix>.tfvars + the Linode API (firewall + cluster by label, the subnet\n" +
			"CIDR from tfvars), replacing the former `terraform init`+`terraform output`\n" +
			"round-trip against the cluster workspace. Explicit env values still win.\n\n" +
			"Env: KUBECONFIG (an existing, non-empty kubeconfig) is always required;\n" +
			"LINODE_FIREWALL_ID is required unless --region resolves it. The Secret token\n" +
			"comes from CLOUD_FIREWALL_TOKEN (preferred least-privilege scope:\n" +
			"firewall:read_write on the node firewall) with LINODE_TOKEN as the\n" +
			"warned-about fallback; CLUSTER_ID is optional. --region label resolution uses\n" +
			"LINODE_TOKEN (or LINODE_API_TOKEN), which needs account read scope.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if region != "" {
				if err := resolveFirewallInputsIntoEnv(region); err != nil {
					return err
				}
			}
			return Run()
		},
	}
	cmd.Flags().StringVar(&region, "region", "", "tfvars prefix (e.g. primary); resolve LINODE_FIREWALL_ID / CLUSTER_ID / VPC_CIDR from <region>.tfvars + the Linode API instead of the environment")
	return cmd
}
