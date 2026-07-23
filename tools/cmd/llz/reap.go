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
	env            string // deployment name; reaps its minted obj-keys + in-cluster PAT
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
	fmt.Printf("  %s%s\n", dim("cluster label: "), orNone(o.clusterLabel))
	fmt.Printf("  %s%s\n\n", dim("env (creds):   "), orNone(o.env))

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

	// ── 6. Per-env minted Linode creds: obj-storage keys + in-cluster PAT ──────
	// These are ACCOUNT-scoped (no cluster tag), so they're keyed off the
	// deployment NAME, not cluster-liveness — a destroyed env's keys are orphaned.
	// Each bootstrap/rotation mints fresh ones under a stable per-env label, and a
	// leaked mint (failed run, failed drain) accretes toward the account's 100-key /
	// 100-PAT caps until a fresh mint 400s. Needs an explicit --env (never a blind
	// account-wide token/key delete).
	fmt.Println("\n" + bold("==== orphan per-env Linode creds (obj-keys + in-cluster PAT) ===="))
	if o.env == "" {
		fmt.Println(dim("  skipped — set --env <deployment> to reap its minted keys + PAT"))
	} else {
		if err := reapEnvObjKeys(ctx, client, o.env, del); err != nil {
			return err
		}
		if err := reapEnvInclusterPAT(ctx, client, o.env, del); err != nil {
			return err
		}
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
	vols, err := client.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("list Volumes: %w", err)
	}
	matched := false
	for _, v := range vols {
		id := linode.MapIDString(v)
		if !linode.VolumeIsCandidate(
			linode.VolumeLinodeIDNull(v), linode.MapString(v, "label"), linode.MapString(v, "region"),
			linode.MapTags(v), o.region, idAllow, id, o.tagMustInclude) {
			continue
		}
		del("/v4/volumes/"+id, fmt.Sprintf("volume %s (%s)", id, linode.MapString(v, "label")))
		matched = true
	}
	if !matched {
		fmt.Println(dim("  none matched the filter"))
	}
	return nil
}

// ── small helpers ────────────────────────────────────────────────────────────

// envObjKeyLabels are the Object Storage key labels the per-env reap targets —
// the obj-key entries buildRotationTable mints for a deployment. A test pins this
// in lockstep with buildRotationTable so a mint-label change can't silently orphan
// the reaper (the exact failure that let 76 keys pile up to the account cap).
func envObjKeyLabels(env string) []string {
	return []string{"platform-loki-" + env, "platform-harbor-registry-" + env, "platform-obj-" + env}
}

// reapEnvObjKeys deletes the Object Storage keys minted for env — the loki +
// harbor-registry keys (labels platform-loki-<env> / platform-harbor-registry-<env>,
// per buildRotationTable). mint-bootstrap-objkeys and the in-cluster rotator each
// create a fresh key under the same stable label; a failed teardown or failed
// grace-window revoke leaks them, and the account caps at 100 keys (a fresh mint
// then 400s "reached your access key quota"). On a destroy the env is gone, so
// every key under those two labels is orphaned. Exact-label match — never another
// env's keys.
func reapEnvObjKeys(ctx context.Context, client *linode.Client, env string, del func(path, desc string)) error {
	keys, err := client.ListObjectStorageKeys(ctx)
	if err != nil {
		return fmt.Errorf("list object-storage keys: %w", err)
	}
	want := map[string]bool{}
	for _, l := range envObjKeyLabels(env) {
		want[l] = true
	}
	for _, k := range keys {
		label := linode.MapString(k, "label")
		if !want[label] {
			continue
		}
		id := linode.MapUint(k, "id")
		del(fmt.Sprintf("/v4/object-storage/keys/%d", id), fmt.Sprintf("obj-key %d (%s)", id, label))
	}
	return nil
}

// reapEnvInclusterPAT deletes the narrow in-cluster PAT(s) minted for env (label
// llz-incluster-<env>, per inclusterPATLabel). mint-bootstrap-pat drains older
// siblings on each mint, but a failed drain / failed run leaks them toward the
// account's 100-PAT cap. Exact-label match — the broad token this sweep RUNS under
// carries a different label, so it is never self-revoked.
func reapEnvInclusterPAT(ctx context.Context, client *linode.Client, env string, del func(path, desc string)) error {
	toks, err := client.ListProfileTokens(ctx)
	if err != nil {
		return fmt.Errorf("list profile tokens: %w", err)
	}
	label := inclusterPATLabel(env)
	for _, t := range toks {
		if linode.MapString(t, "label") != label {
			continue
		}
		id := linode.MapUint(t, "id")
		del(fmt.Sprintf("/v4/profile/tokens/%d", id), fmt.Sprintf("in-cluster PAT %d (%s)", id, label))
	}
	return nil
}

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
