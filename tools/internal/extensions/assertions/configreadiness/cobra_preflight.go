package configreadiness

// cobra_preflight.go — the CLI surface for preflight.
//
// Split from preflight.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func PreflightCmd() *cobra.Command {
	var o preflightOpts
	c := &cobra.Command{
		Use:   "preflight",
		Short: "read-only Linode account capacity / orphan check before a cluster apply",
		Long: "Native port of preflight-quota.sh. Counts current usage + ORPHANED resources\n" +
			"(unattached pvc-* Volumes, CCM NodeBalancers whose cluster is gone, lke<id>\n" +
			"VPCs) — the controllable cause of quota exhaustion — and fails fast so an apply\n" +
			"stops before a 30-minute cluster-create hang. Optional capacity guards\n" +
			"(--cluster-label same-label orphans, --vpc-limit, --vcpu-limit) catch quota\n" +
			"caps up front; limits are operator-supplied (no Linode quota API), unset =\n" +
			"report-only. Reads LINODE_TOKEN; fills --cluster-label/--node-type/--node-count\n" +
			"from <region>.tfvars when run from the cluster TF dir.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIPreflight(o) },
	}
	f := c.Flags()
	f.StringVar(&o.region, "region", "", "narrow the scan to one Linode region (empty = account-wide)")
	f.StringVar(&o.env, "env", "", "deployment name; widens the Volume census to that deployment's RELABELED Volumes (<REGION_SHORT>-<ns>-<pvc>). Without it only the CSI default `pvc-` prefix is counted, so every Volume the volume-labels reconciler has renamed is invisible.")
	f.StringVar(&o.volumeRegion, "volume-region", "", "scope the pvc-* Volume orphan count to one region (empty = the --region value, or account-wide). Volumes carry no cluster id, so an account-wide count flags other regions'/teams' detached Volumes that `llz reap` won't clean — scope to the deployment region to match reap.")
	f.StringVar(&o.failOnOrphans, "fail-on-orphans", "true", "exit non-zero when orphans exceed the threshold (\"true\"/\"false\")")
	f.IntVar(&o.orphanThreshold, "orphan-threshold", 0, "only fail when the orphan count EXCEEDS this")
	f.StringVar(&o.clusterLabel, "cluster-label", "", "the label this apply will create (enables the same-label orphan guard)")
	f.StringVar(&o.nodeType, "node-type", "", "node pool Linode type, for the vCPU estimate (e.g. g6-standard-4)")
	f.IntVar(&o.nodeCount, "node-count", 0, "node pool size, for the vCPU estimate")
	f.IntVar(&o.vpcLimit, "vpc-limit", 0, "account VPC limit; fail if this apply would exceed it (0 = report-only)")
	f.IntVar(&o.vcpuLimit, "vcpu-limit", 0, "account vCPU limit; fail if this apply would exceed it (0 = report-only)")
	return c
}
