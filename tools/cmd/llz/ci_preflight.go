package main

// ci_preflight.go implements `llz ci preflight` — the native port of
// preflight-quota.sh: a read-only account-capacity / orphan scan run BEFORE a
// cluster apply so quota exhaustion fails fast (seconds) instead of as a 30-min
// cluster-create hang. The orphan-identity heuristics are reused from
// internal/linode (the same ones `llz reap` drives); the quota arithmetic is
// internal/preflight; this file is the API orchestration + reporting.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/preflight"
	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
	"github.com/spf13/cobra"
)

type preflightOpts struct {
	region          string
	volumeRegion    string
	failOnOrphans   string // "true" => fail when orphans exceed threshold
	clusterLabel    string
	nodeType        string
	orphanThreshold int
	nodeCount       int
	vpcLimit        int
	vcpuLimit       int
}

func ciPreflightCmd() *cobra.Command {
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

func runCIPreflight(o preflightOpts) error {
	token, err := ciToken()
	if err != nil {
		return err
	}

	// Fall back to <region>.tfvars for the capacity-guard inputs (mirrors the
	// script: the apply-cluster step may run this from the cluster TF dir).
	if o.region != "" {
		if content, rerr := os.ReadFile(o.region + ".tfvars"); rerr == nil {
			v := tf.ParseTFVars(string(content))
			if o.clusterLabel == "" {
				o.clusterLabel = v.ClusterLabel
			}
			if o.nodeType == "" {
				o.nodeType = v.NodeType
			}
			if o.nodeCount == 0 {
				o.nodeCount = v.NodeCount
			}
		}
	}

	client := linode.NewClient(token, 60*time.Second)
	ctx := context.Background()

	fmt.Println(bold(fmt.Sprintf("================ Linode account preflight (region: %s) ================", orAll(o.region))))

	// Same-label capacity signal — >1 live cluster with the label we'll create.
	sameLabel := 0
	if o.clusterLabel != "" {
		ids, err := client.ClustersWithLabel(ctx, o.clusterLabel)
		if err != nil {
			return fmt.Errorf("list clusters by label: %w", err)
		}
		sameLabel = len(ids)
	}

	// Orphan census — the controllable cause of quota exhaustion. Shared with
	// the destroy job's assert-no-orphans gate so the two always agree on what
	// counts as an orphan. NB/VPC are account-wide (cluster-id attributable);
	// Volumes scope to the deployment region (volumeRegion) so the count matches
	// what `llz reap --region <r>` can actually clean — falling back to --region,
	// then account-wide.
	volumeRegion := firstNonEmpty(o.volumeRegion, o.region)
	scan, err := scanOrphans(ctx, client, o.region, volumeRegion)
	if err != nil {
		return err
	}
	orphans := scan.orphans()

	volNote := ""
	if volumeRegion != "" && volumeRegion != o.region {
		volNote = fmt.Sprintf(" [region %s]", volumeRegion)
	}
	labelNote := ""
	if o.clusterLabel != "" {
		labelNote = fmt.Sprintf(" — %d matching %q (e2e)", sameLabel, o.clusterLabel)
	}
	fmt.Printf("  Live LKE clusters : %d total (shared account)%s\n", scan.liveClusters, labelNote)
	fmt.Printf("  Volumes           : %3d total, %3d orphaned (lke<id> cluster gone), %3d untagged detached%s\n", scan.vol.total, scan.vol.orphan, scan.vol.untagged, volNote)
	fmt.Printf("  NodeBalancers     : %3d total, %3d orphaned (lke<id> gone / ccm 0-backend)\n", scan.nb.total, scan.nb.orphan)
	fmt.Printf("  VPCs              : %3d total, %3d orphaned (lke<id>, cluster gone)\n", scan.vpc.total, scan.vpc.orphan)
	orphanCount := fmt.Sprintf("%3d", orphans)
	if orphans > 0 {
		orphanCount = yellow(orphanCount)
	}
	fmt.Printf("  Orphaned total    : %s\n", orphanCount)
	fmt.Println(dim("==================================================================================="))

	// (a) same-label orphans — >1 live cluster with the label we're about to create.
	if preflight.SameLabelExcess(sameLabel) {
		fmt.Fprintf(os.Stderr, "::error::preflight: %d live LKE clusters already carry the label %q. A healthy account has at most one; the rest are orphans from failed/cancelled runs (each holds a VPC + node firewall + nodes). Purge them:\n", sameLabel, o.clusterLabel)
		fmt.Fprintf(os.Stderr, "    LINODE_TOKEN=<token> llz reap --cluster-label %q --region %q --yes\n", o.clusterLabel, o.region)
		return fmt.Errorf("preflight failed: %d clusters share label %q", sameLabel, o.clusterLabel)
	}

	// (b) VPC quota — the confirmed root cause; LKE-E creates one VPC/cluster.
	fmt.Printf("  VPCs in account      : %d total\n  This apply adds      : 1 VPC\n", scan.vpc.total)
	if o.vpcLimit > 0 {
		fmt.Printf("  Account VPC limit    : %d\n", o.vpcLimit)
		if preflight.VPCQuotaExceeded(scan.vpc.total, 1, o.vpcLimit) {
			fmt.Fprintf(os.Stderr, "::error::preflight: account VPC quota would be exceeded — %d existing + 1 for this cluster > %d limit. LKE-E can't allocate the VPC, so cluster-create HANGS. Reap orphaned VPCs (llz reap --region %s) or raise the limit, then retry.\n", scan.vpc.total, o.vpcLimit, o.region)
			return fmt.Errorf("preflight failed: VPC quota would be exceeded (%d + 1 > %d)", scan.vpc.total, o.vpcLimit)
		}
	} else {
		fmt.Println(dim("  (set --vpc-limit to your account's VPC limit to fail fast when an apply would exceed it)"))
	}

	// (c) vCPU quota — secondary; account-wide vCPUs in use + this pool.
	instances, err := client.ListInstances(ctx)
	if err != nil {
		return fmt.Errorf("list Linode instances: %w", err)
	}
	usedVCPU := linode.SumInstanceVCPUs(instances)
	poolVCPU := 0
	if o.nodeType != "" && o.nodeCount > 0 {
		tv, err := client.LinodeTypeVCPUs(ctx, o.nodeType)
		if err != nil {
			return fmt.Errorf("look up Linode type %q: %w", o.nodeType, err)
		}
		poolVCPU = preflight.PoolVCPU(tv, o.nodeCount)
	}
	fmt.Printf("  Account vCPUs in use : %d (all teams — shared account)\n", usedVCPU)
	fmt.Printf("  This apply adds      : %d vCPU (%s x %s)\n", poolVCPU, orQ(strconv.Itoa(o.nodeCount), o.nodeCount == 0), orQ(o.nodeType, o.nodeType == ""))
	if o.vcpuLimit > 0 {
		fmt.Printf("  Account vCPU limit   : %d\n", o.vcpuLimit)
		if preflight.VCPUQuotaExceeded(usedVCPU, poolVCPU, o.vcpuLimit) {
			fmt.Fprintf(os.Stderr, "::error::preflight: account vCPU quota would be exceeded — %d in use + %d requested > %d limit. The new node pool can't provision, so cluster-create HANGS. Free capacity or raise the limit, then retry.\n", usedVCPU, poolVCPU, o.vcpuLimit)
			return fmt.Errorf("preflight failed: vCPU quota would be exceeded (%d + %d > %d)", usedVCPU, poolVCPU, o.vcpuLimit)
		}
	} else {
		fmt.Println(dim("  (set --vcpu-limit to your account's vCPU limit to fail fast when an apply would exceed it)"))
	}

	// Untagged detached Volumes: ADVISORY only — never gates. They consume account
	// quota but carry no lke<id> tag, so they can't be attributed to a gone cluster
	// and may be an intentionally-detached Retain PVC. Surface them for cleanup
	// awareness; clearing is a deliberate opt-in.
	if scan.vol.untagged > 0 {
		fmt.Fprintf(os.Stderr, "::warning::preflight: %d untagged detached pvc-* Volume(s)%s consume quota but carry no lke<id> tag — NOT gated on (could be an intentionally-detached PVC). If leaked, clear deliberately: `llz ci reap-volumes --region <r> --reap-untagged`.\n", scan.vol.untagged, volNote)
	}

	// (d) DEFINITIVE orphans over threshold — resources attributable to a GONE
	// cluster (gone-cluster-tagged Volumes + NodeBalancers + VPCs). Untagged Volumes
	// are advisory only (above) and never contribute to this gate.
	if preflight.OrphansExceedThreshold(orphans, o.orphanThreshold) {
		fmt.Fprintf(os.Stderr, "::warning::preflight: %d orphaned Linode resource(s) whose owning cluster is GONE (threshold %d). These count against the account's active-services quota and will stall a fresh apply. Clean up: llz reap (account-wide) or llz ci reap-volumes / reap-nodebalancers.\n", orphans, o.orphanThreshold)
		if o.failOnOrphans == "true" {
			fmt.Fprintln(os.Stderr, "::error::preflight failed: clear the gone-cluster orphans above, then re-run.")
			return fmt.Errorf("preflight failed: %d gone-cluster orphan(s) over threshold %d", orphans, o.orphanThreshold)
		}
		fmt.Println(yellow("--fail-on-orphans=false — continuing despite orphans (report-only)."))
		return nil
	}

	fmt.Printf("%s Preflight OK — no orphaned resources above threshold; account has capacity to proceed.\n", green("✓"))
	return nil
}

// orQ renders a value, or "?" when it's the unknown/zero case (display only).
func orQ(s string, unknown bool) string {
	if unknown {
		return "?"
	}
	return s
}

// orphanScanner is the slice of the Linode client the orphan census needs —
// seamed so scanOrphans (and the destroy-job assert-no-orphans gate) is
// unit-testable without the live API.
type orphanScanner interface {
	LiveClusterIDs(ctx context.Context) (map[string]bool, error)
	ListVolumes(ctx context.Context) ([]map[string]any, error)
	ListNodeBalancers(ctx context.Context) ([]map[string]any, error)
	NodeBalancerBackendCount(ctx context.Context, id uint64) (int, error)
	ListVPCs(ctx context.Context) ([]map[string]any, error)
}

// resourceTally is per-type total + orphan counts for the census report.
// `orphan` is a DEFINITIVE orphan: a resource attributable to a cluster that is
// GONE (a gone-cluster `lke<id>` tag/label). `untagged` is volume-only — detached
// pvc-* Volumes carrying no `lke<id>` tag: they consume quota but can't be
// attributed to a gone cluster (may be an intentionally-detached Retain PVC), so
// they are reported but NEVER gated on. Only `orphan` feeds orphans().
type resourceTally struct{ total, orphan, untagged int }

// orphanScan is the account- (or region-) scoped orphan census that both
// `llz ci preflight` reports and the destroy job's gate asserts on.
type orphanScan struct {
	liveClusters int
	vol, nb, vpc resourceTally
}

func (s orphanScan) orphans() int { return s.vol.orphan + s.nb.orphan + s.vpc.orphan }

// scanOrphans counts DEFINITIVE orphans (resources attributable to a GONE cluster)
// using the SAME identity heuristics `llz reap` drives — gone-cluster-tagged pvc-*
// Volumes, CCM NodeBalancers whose cluster is gone (or 0-backend), and lke<id> VPCs
// whose cluster is gone — plus, SEPARATELY, a `vol.untagged` count of detached pvc-*
// Volumes with no lke<id> tag (advisory: quota-relevant but unattributable, so never
// gated on). NodeBalancers and VPCs are scoped to region ("" = account-wide): they
// carry a cluster-id tag/label, so a gone-cluster orphan is unambiguous and safe to
// count account-wide. Volumes stamped at provision by the block-storage-retain
// StorageClass carry an lke<id> cluster tag, so a detached Volume of a still-live
// cluster is excluded (VolKeep) and a gone cluster's is a definitive orphan — but
// volumeRegion scoping is still applied because UNtagged legacy Volumes carry no
// cluster id and can't be attributed: in a shared account an account-wide count
// would pull in other regions'/teams' detached Volumes that `llz reap` (refuses an unscoped
// Volume sweep and only acts per --region) will never clean, so the gate would
// disagree with reap. volumeRegion="" preserves the account-wide volume count.
// Read-only.
func scanOrphans(ctx context.Context, client orphanScanner, region, volumeRegion string) (orphanScan, error) {
	inRegion := func(r string) bool { return region == "" || region == r }
	inVolumeRegion := func(r string) bool { return volumeRegion == "" || volumeRegion == r }

	live, err := client.LiveClusterIDs(ctx)
	if err != nil {
		return orphanScan{}, fmt.Errorf("list LKE clusters: %w", err)
	}
	s := orphanScan{liveClusters: len(live)}
	now := time.Now()

	vols, err := client.ListVolumes(ctx)
	if err != nil {
		return orphanScan{}, fmt.Errorf("list Volumes: %w", err)
	}
	for _, v := range vols {
		if !inVolumeRegion(linode.MapString(v, "region")) {
			continue
		}
		s.vol.total++
		tags := linode.MapTags(v)
		// A detached `pvc-*` Volume in scope, classified by the cluster-liveness gate:
		//   VolKeep     — tagged `lke<id>` for a still-LIVE cluster: a Retain Volume
		//                 in use, never an orphan (reap keeps it too).
		//   VolOrphan   — tagged `lke<id>` for a GONE cluster: a DEFINITIVE orphan
		//                 (its PVC's cluster was deleted). Counted, gated on.
		//   VolUntagged — no `lke<id>` tag: unattributable. Past the tagging grace
		//                 it's counted SEPARATELY (untagged, not orphan): it consumes
		//                 quota, but with no cluster signal it can't be proven a
		//                 gone-cluster leak — it may be an intentionally-detached PVC,
		//                 so the gate must NOT fail on it. (Young untagged Volumes are
		//                 excluded entirely — a live cluster's Volume mid-provision.)
		if linode.VolumeIsCandidate(linode.VolumeLinodeIDNull(v), linode.MapString(v, "label"),
			linode.MapString(v, "region"), tags, volumeRegion, nil, linode.MapIDString(v), "") {
			switch linode.ClassifyVolume(tags, live) {
			case linode.VolKeep:
				// in use — not an orphan
			case linode.VolUntagged:
				if !linode.VolumeYoungerThan(linode.MapString(v, "created"), now, linode.UntaggedVolumeReapGrace) {
					s.vol.untagged++
				}
			default: // VolOrphan — gone cluster, definitive orphan
				s.vol.orphan++
			}
		}
	}

	nbs, err := client.ListNodeBalancers(ctx)
	if err != nil {
		return orphanScan{}, fmt.Errorf("list NodeBalancers: %w", err)
	}
	for _, nb := range nbs {
		if !inRegion(linode.MapString(nb, "region")) {
			continue
		}
		s.nb.total++
		switch linode.ClassifyNodeBalancer(linode.LKEClusterIDFromNB(nb), linode.MapTags(nb), linode.MapString(nb, "label"), live) {
		case linode.NBKeep:
			continue
		case linode.NBCheckBackends:
			n, err := client.NodeBalancerBackendCount(ctx, linode.MapUint(nb, "id"))
			if err != nil || n != 0 {
				continue
			}
		}
		s.nb.orphan++
	}

	vpcs, err := client.ListVPCs(ctx)
	if err != nil {
		return orphanScan{}, fmt.Errorf("list VPCs: %w", err)
	}
	for _, vpc := range vpcs {
		if !inRegion(linode.MapString(vpc, "region")) {
			continue
		}
		s.vpc.total++
		if linode.VPCIsOrphan(linode.MapString(vpc, "label"), live) {
			s.vpc.orphan++
		}
	}
	return s, nil
}
