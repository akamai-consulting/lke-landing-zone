package teardown

// orphans.go — what counts as an ORPHAN, defined once for the three things that
// disagree expensively if they drift.
//
// `llz ci preflight` REPORTS this census before a run, `assert-no-orphans` GATES
// on it after a destroy, and `llz reap` CLEANS by the same identity heuristics.
// The comment this block carried in package main said it outright — "shared with
// the destroy job's assert-no-orphans gate so the two always agree on what counts
// as an orphan" — and while all three lived in one package that agreement was
// upheld by nothing but proximity.
//
// It lives HERE, with teardown, because this is where the definition has teeth: a
// census that under-counts makes preflight optimistic, but it makes the destroy
// gate PASS on a leak. The volume-prefix widening in ScanOrphans is exactly that
// bug, already paid for once.

import (
	"context"
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// OrphanScanner is the slice of the Linode client the orphan census needs —
// seamed so ScanOrphans (and the destroy-job assert-no-Orphans gate) is
// unit-testable without the live API.
type OrphanScanner interface {
	LiveClusterIDs(ctx context.Context) (map[string]bool, error)
	ListVolumes(ctx context.Context) ([]map[string]any, error)
	ListNodeBalancers(ctx context.Context) ([]map[string]any, error)
	NodeBalancerBackendCount(ctx context.Context, id uint64) (int, error)
	ListVPCs(ctx context.Context) ([]map[string]any, error)
}

// ResourceTally is per-type total + orphan counts for the census report.
type ResourceTally struct{ Total, Orphan int }

// OrphanScan is the account- (or region-) scoped orphan census that both
// `llz ci preflight` reports and the destroy job's gate asserts on.
type OrphanScan struct {
	LiveClusters int
	Vol, NB, VPC ResourceTally
}

func (s OrphanScan) Orphans() int { return s.Vol.Orphan + s.NB.Orphan + s.VPC.Orphan }

// ScanOrphans counts orphaned Volumes / NodeBalancers / VPCs using the SAME
// identity heuristics `llz reap` drives — unattached pvc-* Volumes, CCM
// NodeBalancers whose cluster is gone (or 0-backend), and lke<id> VPCs whose
// cluster is gone. NodeBalancers and VPCs are scoped to region ("" =
// account-wide): they carry a cluster-id tag/label, so a gone-cluster orphan is
// unambiguous and safe to count account-wide. Volumes are scoped SEPARATELY to
// volumeRegion because a detached relabeled Volume carries no cluster id and can't
// be attributed — in a shared account an account-wide count pulls in other
// regions'/teams' detached Volumes that `llz reap` (which refuses an unscoped
// Volume sweep and only acts per --region) will never clean, so the gate would
// disagree with reap. volumeRegion="" preserves the account-wide volume count.
// env is the DEPLOYMENT name and widens the Volume filter to that deployment's
// RELABELED Volumes. Without it the census accepts only the CSI default "pvc-"
// prefix, so every Volume the volume-labels reconciler has renamed — all of them,
// on a converged cluster — is invisible to the count. That is not a cosmetic gap:
// assert-no-Orphans is the destroy job's final gate, so a leak of exactly the
// Volumes this deployment created would report zero Orphans and pass.
// Read-only.
func ScanOrphans(ctx context.Context, client OrphanScanner, region, volumeRegion, env string) (OrphanScan, error) {
	inRegion := func(r string) bool { return region == "" || region == r }
	inVolumeRegion := func(r string) bool { return volumeRegion == "" || volumeRegion == r }
	volPrefixes := linode.VolumeLabelPrefixes(env)

	live, err := client.LiveClusterIDs(ctx)
	if err != nil {
		return OrphanScan{}, fmt.Errorf("list LKE clusters: %w", err)
	}
	s := OrphanScan{LiveClusters: len(live)}

	vols, err := client.ListVolumes(ctx)
	if err != nil {
		return OrphanScan{}, fmt.Errorf("list Volumes: %w", err)
	}
	for _, v := range vols {
		if !inVolumeRegion(linode.MapString(v, "region")) {
			continue
		}
		s.Vol.Total++
		if linode.VolumeIsCandidate(linode.VolumeLinodeIDNull(v), linode.MapString(v, "label"),
			linode.MapString(v, "region"), linode.MapTags(v), volumeRegion, nil, linode.MapIDString(v), "",
			volPrefixes...) {
			s.Vol.Orphan++
		}
	}

	nbs, err := client.ListNodeBalancers(ctx)
	if err != nil {
		return OrphanScan{}, fmt.Errorf("list NodeBalancers: %w", err)
	}
	for _, nb := range nbs {
		if !inRegion(linode.MapString(nb, "region")) {
			continue
		}
		s.NB.Total++
		switch linode.ClassifyNodeBalancer(linode.LKEClusterIDFromNB(nb), linode.MapTags(nb), linode.MapString(nb, "label"), live) {
		case linode.NBKeep:
			continue
		case linode.NBCheckBackends:
			n, err := client.NodeBalancerBackendCount(ctx, linode.MapUint(nb, "id"))
			if err != nil || n != 0 {
				continue
			}
		}
		s.NB.Orphan++
	}

	vpcs, err := client.ListVPCs(ctx)
	if err != nil {
		return OrphanScan{}, fmt.Errorf("list VPCs: %w", err)
	}
	for _, vpc := range vpcs {
		if !inRegion(linode.MapString(vpc, "region")) {
			continue
		}
		s.VPC.Total++
		if linode.VPCIsOrphan(linode.MapString(vpc, "label"), live) {
			s.VPC.Orphan++
		}
	}
	return s, nil
}
