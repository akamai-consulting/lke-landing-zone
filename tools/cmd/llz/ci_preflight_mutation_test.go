package main

import (
	"context"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/teardown"
)

// The census reports "N of M" per resource type: the orphan count is only
// meaningful against a total that actually counts every resource the scan
// considered. The Volume total additionally follows the SEPARATE volumeRegion
// scope (preflight must agree with what `llz reap --region` would clean), while
// NodeBalancers and VPCs follow --region.
func TestScanOrphansTotalsFollowTheirOwnRegionScope(t *testing.T) {
	fake := &fakeOrphanScanner{
		live: map[string]bool{"100": true},
		volumes: []map[string]any{
			{"id": float64(1), "label": "pvc-a", "region": "us-ord", "linode_id": nil},
			{"id": float64(2), "label": "pvc-b", "region": "us-ord", "linode_id": float64(5)},
			{"id": float64(3), "label": "pvc-c", "region": "us-east", "linode_id": nil},
		},
		nbs: []map[string]any{
			{"id": float64(10), "label": "nb-live", "region": "us-ord", "tags": []any{"lke100"}},
			{"id": float64(11), "label": "nb-gone", "region": "us-ord", "tags": []any{"lke999"}},
		},
		vpcs: []map[string]any{
			{"id": float64(20), "label": "lke100", "region": "us-ord"},
			{"id": float64(21), "label": "lke999", "region": "us-ord"},
			{"id": float64(22), "label": "lke100", "region": "us-east"},
		},
	}
	ctx := context.Background()

	// Account-wide: every resource is counted.
	all, err := teardown.ScanOrphans(ctx, fake, "", "", "")
	if err != nil {
		t.Fatalf("scanOrphans: %v", err)
	}
	if all.Vol.Total != 3 || all.NB.Total != 2 || all.VPC.Total != 3 {
		t.Errorf("account-wide totals = vol %d / nb %d / vpc %d, want 3/2/3",
			all.Vol.Total, all.NB.Total, all.VPC.Total)
	}
	if all.LiveClusters != 1 {
		t.Errorf("liveClusters = %d, want 1", all.LiveClusters)
	}

	// volumeRegion narrows ONLY the Volume total, and to that region's Volumes.
	scoped, err := teardown.ScanOrphans(ctx, fake, "", "us-ord", "")
	if err != nil {
		t.Fatalf("teardown.ScanOrphans(volumeRegion): %v", err)
	}
	if scoped.Vol.Total != 2 {
		t.Errorf("us-ord volume total = %d, want 2 (the two us-ord Volumes)", scoped.Vol.Total)
	}
	if scoped.NB.Total != 2 || scoped.VPC.Total != 3 {
		t.Errorf("volumeRegion must not narrow NB/VPC: nb %d / vpc %d, want 2/3",
			scoped.NB.Total, scoped.VPC.Total)
	}

	// --region narrows NodeBalancers and VPCs.
	east, err := teardown.ScanOrphans(ctx, fake, "us-east", "", "")
	if err != nil {
		t.Fatalf("teardown.ScanOrphans(region): %v", err)
	}
	if east.NB.Total != 0 || east.VPC.Total != 1 {
		t.Errorf("us-east totals = nb %d / vpc %d, want 0/1", east.NB.Total, east.VPC.Total)
	}
}
