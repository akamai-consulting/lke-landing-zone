package main

// reap.go is the operator-facing orchestrator for `llz reap` — the native port of
// reap-all-orphaned-resources.sh: a one-shot manual sweep of Linode resources
// leaked by failed/cancelled cluster cycles, run in dependency order (clusters →
// firewall → NodeBalancers → VPCs → Volumes). The orphan-identity heuristics +
// API primitives live in internal/linode (reap.go); this file is control flow,
// dry-run gating, and output. Dry-run by default; deletes only with --yes.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

type reapOpts struct {
	region         string
	clusterLabel   string
	fwLabel        string
	volumeIDs      string // space-separated allowlist
	tagMustInclude string
	force          bool
}

func runReap(g globalOpts, o reapOpts) error {
	token := firstNonEmpty(os.Getenv("LINODE_API_TOKEN"), os.Getenv("LINODE_TOKEN"))
	if token == "" {
		return fmt.Errorf("set LINODE_API_TOKEN (or LINODE_TOKEN) to a Linode PAT (read_write to delete, read_only for a dry-run)")
	}
	confirm := g.yes && !g.dryRun
	client := linode.NewClient(token, 60*time.Second)
	ctx := context.Background()

	fmt.Println(bold("################ llz reap — orphaned Linode resources ################"))
	if !confirm {
		fmt.Println(yellow("DRY-RUN — nothing will be deleted. Re-run with --yes to delete."))
	}
	fmt.Printf("  %s%s\n", dim("region:        "), orAll(o.region))
	fmt.Printf("  %s%s\n\n", dim("cluster label: "), orNone(o.clusterLabel))

	deleted, failed := 0, 0
	// del prints (dry-run) or deletes (confirm), tallying outcomes.
	del := func(path, desc string) {
		if !confirm {
			fmt.Printf("  %s %s\n", cyan("would DELETE"), desc)
			return
		}
		if err := client.DeleteResourcePath(ctx, path); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", red(fmt.Sprintf("DELETE %s FAILED", desc)), err)
			failed++
			return
		}
		fmt.Printf("  %s %s\n", green("DELETE"), desc)
		deleted++
	}

	// ── 1. Orphan clusters by label (root) ───────────────────────────────────
	clustersDeleted := false
	if o.clusterLabel != "" {
		fmt.Println(bold(fmt.Sprintf("==== orphan clusters (label %q) ====", o.clusterLabel)))
		ids, err := client.ClustersWithLabel(ctx, o.clusterLabel)
		if err != nil {
			return fmt.Errorf("list clusters: %w", err)
		}
		for _, id := range ids {
			del(fmt.Sprintf("/v4beta/lke/clusters/%d", id), fmt.Sprintf("cluster %d", id))
			clustersDeleted = true
		}
		if len(ids) == 0 {
			fmt.Println(dim("  none matched"))
		}
		// Cluster delete is async; let it settle so the firewall safety guard
		// (which refuses while a live cluster still carries the label) passes.
		if confirm && clustersDeleted {
			fmt.Println(dim("  (waiting 25s for cluster delete to settle)"))
			time.Sleep(25 * time.Second)
		}

		// ── 2. Orphan node firewall ──────────────────────────────────────────
		fmt.Println("\n" + bold("==== orphan node firewall ===="))
		if err := reapFirewalls(ctx, client, o, del); err != nil {
			return err
		}
	} else {
		fmt.Println(bold("==== orphan clusters + firewall ====") + dim(" — skipped (no --cluster-label)"))
	}

	// ── 3. NodeBalancers BEFORE VPCs (a parked NB blocks its VPC delete) ──────
	fmt.Println("\n" + bold("==== orphan NodeBalancers (account-wide) ===="))
	if err := reapNodeBalancers(ctx, client, o, del); err != nil {
		return err
	}

	// ── 4. VPCs (lke<id> cluster-gone, + <label>-vpc when --cluster-label) ────
	fmt.Println("\n" + bold("==== orphan VPCs ===="))
	if err := reapVPCs(ctx, client, o, del); err != nil {
		return err
	}

	// ── 5. Volumes (needs a scope: --region or --volume-ids) ──────────────────
	fmt.Println("\n" + bold("==== orphan Volumes ===="))
	if o.region == "" && o.volumeIDs == "" {
		fmt.Println(dim("  skipped — set --region and/or --volume-ids to scope the sweep (refusing an unscoped Volume delete)"))
	} else if err := reapVolumes(ctx, client, o, del); err != nil {
		return err
	}

	summary := fmt.Sprintf("summary: deleted=%d failed=%d", deleted, failed)
	if failed > 0 {
		summary = red(summary)
	} else if deleted > 0 {
		summary = green(summary)
	}
	fmt.Printf("\n%s\n", summary)
	if !confirm {
		fmt.Println(dim("(dry-run — nothing was deleted; re-run with --yes)"))
	}
	if failed > 0 {
		return fmt.Errorf("%d delete(s) failed", failed)
	}
	return nil
}

func reapFirewalls(ctx context.Context, client *linode.Client, o reapOpts, del func(path, desc string)) error {
	// Candidate labels (account-unique, so each matches ≤1 firewall).
	var candidates []string
	if o.fwLabel != "" {
		candidates = []string{o.fwLabel}
	} else {
		candidates = []string{"platform-nodes-fw", truncate(o.clusterLabel, 26) + "-nodes"}
	}
	// Safety: never delete a live cluster's firewall.
	if !o.force {
		live, err := client.ClustersWithLabel(ctx, o.clusterLabel)
		if err != nil {
			return fmt.Errorf("firewall safety check: %w", err)
		}
		if len(live) > 0 {
			fmt.Printf("  %s\n", yellow(fmt.Sprintf("a live cluster still carries label %q — refusing (delete the cluster first, or --force)", o.clusterLabel)))
			return nil
		}
	}
	fws, err := client.ListFirewalls(ctx)
	if err != nil {
		return fmt.Errorf("list firewalls: %w", err)
	}
	matched := false
	for _, fw := range fws {
		label := linode.MapString(fw, "label")
		if !containsString(candidates, label) {
			continue
		}
		id := linode.MapUint(fw, "id")
		del(fmt.Sprintf("/v4/networking/firewalls/%d", id), fmt.Sprintf("firewall %d (%s)", id, label))
		matched = true
	}
	if !matched {
		fmt.Printf("%s\n", dim(fmt.Sprintf("  none matched (searched: %s)", strings.Join(candidates, ", "))))
	}
	return nil
}

func reapNodeBalancers(ctx context.Context, client *linode.Client, o reapOpts, del func(path, desc string)) error {
	live, err := client.LiveClusterIDs(ctx)
	if err != nil {
		return fmt.Errorf("load live clusters: %w", err)
	}
	nbs, err := client.ListNodeBalancers(ctx)
	if err != nil {
		return fmt.Errorf("list NodeBalancers: %w", err)
	}
	matched := false
	for _, nb := range nbs {
		region := linode.MapString(nb, "region")
		if o.region != "" && region != o.region {
			continue
		}
		tags := linode.MapTags(nb)
		label := linode.MapString(nb, "label")
		switch linode.ClassifyNodeBalancer(linode.LKEClusterIDFromNB(nb), tags, label, live) {
		case linode.NBKeep:
			continue
		case linode.NBCheckBackends:
			n, err := client.NodeBalancerBackendCount(ctx, linode.MapUint(nb, "id"))
			if err != nil || n != 0 {
				continue
			}
		}
		id := linode.MapUint(nb, "id")
		del(fmt.Sprintf("/v4/nodebalancers/%d", id),
			fmt.Sprintf("nodebalancer %d (%s, %s)", id, label, region))
		matched = true
	}
	if !matched {
		fmt.Println(dim("  none matched"))
	}
	return nil
}

func reapVPCs(ctx context.Context, client *linode.Client, o reapOpts, del func(path, desc string)) error {
	live, err := client.LiveClusterIDs(ctx)
	if err != nil {
		return fmt.Errorf("load live clusters: %w", err)
	}
	byoLabel := ""
	if o.clusterLabel != "" {
		held, err := client.ClustersWithLabel(ctx, o.clusterLabel)
		if err != nil {
			return err
		}
		if len(held) > 0 {
			fmt.Printf("  %s\n", yellow(fmt.Sprintf("a live cluster still carries label %q — not targeting its %q VPC", o.clusterLabel, o.clusterLabel+"-vpc")))
		} else {
			byoLabel = o.clusterLabel + "-vpc"
		}
	}
	vpcs, err := client.ListVPCs(ctx)
	if err != nil {
		return fmt.Errorf("list VPCs: %w", err)
	}
	matched := false
	for _, vpc := range vpcs {
		region := linode.MapString(vpc, "region")
		if o.region != "" && region != o.region {
			continue
		}
		label := linode.MapString(vpc, "label")
		id := linode.MapUint(vpc, "id")
		isOrphan := linode.VPCIsOrphan(label, live)
		if !isOrphan && !(byoLabel != "" && label == byoLabel) {
			continue
		}
		// Subnets must go before the VPC.
		subs, err := client.ListVPCSubnets(ctx, id)
		if err != nil {
			return fmt.Errorf("list subnets of vpc %d: %w", id, err)
		}
		for _, s := range subs {
			sid := linode.MapUint(s, "id")
			del(fmt.Sprintf("/v4/vpcs/%d/subnets/%d", id, sid), fmt.Sprintf("vpc %d subnet %d", id, sid))
		}
		del(fmt.Sprintf("/v4/vpcs/%d", id), fmt.Sprintf("vpc %d (%s)", id, label))
		matched = true
	}
	if !matched {
		fmt.Println(dim("  none matched"))
	}
	return nil
}

func reapVolumes(ctx context.Context, client *linode.Client, o reapOpts, del func(path, desc string)) error {
	idAllow := map[string]bool{}
	for _, id := range strings.Fields(o.volumeIDs) {
		idAllow[id] = true
	}
	// Cluster-liveness gate: every PVC the block-storage StorageClass provisions
	// carries an `lke<id>` tag for its owning cluster, so a detached `pvc-*` Volume
	// whose cluster is still live is a Retain Volume in use — NOT an orphan — and
	// must be kept. We only load the live set for a broad (region) sweep: an
	// explicit --volume-ids allowlist is a deliberate, precise scope the caller
	// owns (e.g. CI tearing down one cluster's Volumes), so it bypasses the gate.
	var live map[string]bool
	if len(idAllow) == 0 {
		var err error
		if live, err = client.LiveClusterIDs(ctx); err != nil {
			return fmt.Errorf("load live clusters: %w", err)
		}
	}
	vols, err := client.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("list Volumes: %w", err)
	}
	matched, keptLive := false, 0
	for _, v := range vols {
		id := linode.MapIDString(v)
		tags := linode.MapTags(v)
		if !linode.VolumeIsCandidate(
			linode.VolumeLinodeIDNull(v), linode.MapString(v, "label"), linode.MapString(v, "region"),
			tags, o.region, idAllow, id, o.tagMustInclude) {
			continue
		}
		if len(idAllow) == 0 && linode.ClassifyVolume(tags, live) == linode.VolKeep {
			keptLive++
			continue
		}
		del("/v4/volumes/"+id, fmt.Sprintf("volume %s (%s)", id, linode.MapString(v, "label")))
		matched = true
	}
	if keptLive > 0 {
		fmt.Println(dim(fmt.Sprintf("  kept %d detached Volume(s) tagged to a live cluster (not orphans)", keptLive)))
	}
	if !matched {
		fmt.Println(dim("  none matched the filter"))
	}
	return nil
}

// ── small helpers ────────────────────────────────────────────────────────────

func orAll(s string) string {
	if s == "" {
		return "(all)"
	}
	return s
}
func orNone(s string) string {
	if s == "" {
		return "(none — skipping cluster/firewall/BYO-VPC steps)"
	}
	return s
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
