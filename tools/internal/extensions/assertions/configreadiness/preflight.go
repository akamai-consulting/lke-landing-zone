package configreadiness

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/preflight"
	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/terraform"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/teardown"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

type preflightOpts struct {
	region          string
	env             string
	volumeRegion    string
	failOnOrphans   string // "true" => fail when orphans exceed threshold
	clusterLabel    string
	nodeType        string
	orphanThreshold int
	nodeCount       int
	vpcLimit        int
	vcpuLimit       int
}

func runCIPreflight(o preflightOpts) error {
	token, err := deps.CloudToken()
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

	fmt.Println(color.Bold(fmt.Sprintf("================ Linode account preflight (region: %s) ================", orAll(o.region))))

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
	scan, err := teardown.ScanOrphans(ctx, client, o.region, volumeRegion, o.env)
	if err != nil {
		return err
	}
	orphans := scan.Orphans()

	volNote := ""
	if volumeRegion != "" && volumeRegion != o.region {
		volNote = fmt.Sprintf(" [region %s]", volumeRegion)
	}
	labelNote := ""
	if o.clusterLabel != "" {
		labelNote = fmt.Sprintf(" — %d matching %q (e2e)", sameLabel, o.clusterLabel)
	}
	fmt.Printf("  Live LKE clusters : %d total (shared account)%s\n", scan.LiveClusters, labelNote)
	fmt.Printf("  Volumes           : %3d total, %3d orphaned (unattached pvc-*)%s\n", scan.Vol.Total, scan.Vol.Orphan, volNote)
	fmt.Printf("  NodeBalancers     : %3d total, %3d orphaned (lke<id> gone / ccm 0-backend)\n", scan.NB.Total, scan.NB.Orphan)
	fmt.Printf("  VPCs              : %3d total, %3d orphaned (lke<id>, cluster gone)\n", scan.VPC.Total, scan.VPC.Orphan)
	orphanCount := fmt.Sprintf("%3d", orphans)
	if orphans > 0 {
		orphanCount = color.Yellow(orphanCount)
	}
	fmt.Printf("  Orphaned total    : %s\n", orphanCount)
	fmt.Println(color.Dim("==================================================================================="))

	// (a) same-label orphans — >1 live cluster with the label we're about to create.
	if preflight.SameLabelExcess(sameLabel) {
		fmt.Fprintf(os.Stderr, "::error::preflight: %d live LKE clusters already carry the label %q. A healthy account has at most one; the rest are orphans from failed/cancelled runs (each holds a VPC + node firewall + nodes). Purge them:\n", sameLabel, o.clusterLabel)
		fmt.Fprintf(os.Stderr, "    LINODE_TOKEN=<token> llz reap --cluster-label %q --region %q --yes\n", o.clusterLabel, o.region)
		return fmt.Errorf("preflight failed: %d clusters share label %q", sameLabel, o.clusterLabel)
	}

	// (b) VPC quota — the confirmed root cause; LKE-E creates one VPC/cluster.
	fmt.Printf("  VPCs in account      : %d total\n  This apply adds      : 1 VPC\n", scan.VPC.Total)
	if o.vpcLimit > 0 {
		fmt.Printf("  Account VPC limit    : %d\n", o.vpcLimit)
		if preflight.VPCQuotaExceeded(scan.VPC.Total, 1, o.vpcLimit) {
			fmt.Fprintf(os.Stderr, "::error::preflight: account VPC quota would be exceeded — %d existing + 1 for this cluster > %d limit. LKE-E can't allocate the VPC, so cluster-create HANGS. Reap orphaned VPCs (llz reap --region %s) or raise the limit, then retry.\n", scan.VPC.Total, o.vpcLimit, o.region)
			return fmt.Errorf("preflight failed: VPC quota would be exceeded (%d + 1 > %d)", scan.VPC.Total, o.vpcLimit)
		}
	} else {
		fmt.Println(color.Dim("  (set --vpc-limit to your account's VPC limit to fail fast when an apply would exceed it)"))
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
		fmt.Println(color.Dim("  (set --vcpu-limit to your account's vCPU limit to fail fast when an apply would exceed it)"))
	}

	// (d) orphans over threshold.
	if preflight.OrphansExceedThreshold(orphans, o.orphanThreshold) {
		fmt.Fprintf(os.Stderr, "::warning::preflight: %d orphaned Linode resource(s) detected (threshold %d). These count against the account's active-services quota and will stall a fresh apply. Clean up first: llz reap (account-wide) or llz ci reap-volumes / reap-nodebalancers.\n", orphans, o.orphanThreshold)
		if o.failOnOrphans == "true" {
			fmt.Fprintln(os.Stderr, "::error::preflight failed: clear the orphans above, then re-run.")
			return fmt.Errorf("preflight failed: %d orphaned resource(s) over threshold %d", orphans, o.orphanThreshold)
		}
		fmt.Println(color.Yellow("--fail-on-orphans=false — continuing despite orphans (report-only)."))
		return nil
	}

	fmt.Printf("%s Preflight OK — no orphaned resources above threshold; account has capacity to proceed.\n", color.Green("✓"))
	return nil
}

// orQ renders a value, or "?" when it's the unknown/zero case (display only).
func orQ(s string, unknown bool) string {
	if unknown {
		return "?"
	}
	return s
}
