package teardown

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/instanceresolve"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/objenc"
)

type ReapOpts struct {
	Region         string
	ClusterLabel   string
	FwLabel        string
	VolumeIDs      string // space-separated allowlist
	TagMustInclude string
	Env            string // deployment name; reaps its minted obj-keys + in-cluster PAT
	Force          bool
}

func RunReap(dryRun, yes bool, o ReapOpts) error {
	token := firstNonEmpty(os.Getenv("LINODE_API_TOKEN"), os.Getenv("LINODE_TOKEN"))
	if token == "" {
		return fmt.Errorf("set LINODE_API_TOKEN (or LINODE_TOKEN) to a Linode PAT (read_write to delete, read_only for a dry-run)")
	}
	confirm := yes && !dryRun
	client := linode.NewClient(token, 60*time.Second)
	ctx := context.Background()

	// `--region` here is a LINODE region (us-ord), unlike almost every `llz ci`
	// verb, where --region is the DEPLOYMENT name. That inconsistency is a trap
	// with a silent failure on the other side: the sweeps compare it verbatim
	// against each resource's region (reapNodeBalancers/reapVPCs/reapVolumes), so
	// a deployment name matches nothing, every section prints "none matched", and
	// the run ends `deleted=0` — which an operator reads as "the account is
	// clean". This is the recovery path the first-build-failed runbook sends
	// people to, so a false all-clear here costs them the orphan backlog that
	// hangs their next cluster-create.
	if err := checkReapRegion(o.Region); err != nil {
		return err
	}

	fmt.Println(color.Bold("################ llz reap — orphaned Linode resources ################"))
	if !confirm {
		fmt.Println(color.Yellow("DRY-RUN — nothing will be deleted. Re-run with --yes to delete."))
	}
	fmt.Printf("  %s%s\n", color.Dim("Region:        "), orAll(o.Region))
	fmt.Printf("  %s%s\n", color.Dim("cluster label: "), orNone(o.ClusterLabel))
	fmt.Printf("  %s%s\n\n", color.Dim("env (creds):   "), orNone(o.Env))

	deleted, failed := 0, 0
	// del prints (dry-run) or deletes (confirm), tallying outcomes.
	del := func(path, desc string) {
		if !confirm {
			fmt.Printf("  %s %s\n", color.Cyan("would DELETE"), desc)
			return
		}
		if err := client.DeleteResourcePath(ctx, path); err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", color.Red(fmt.Sprintf("DELETE %s FAILED", desc)), err)
			failed++
			return
		}
		fmt.Printf("  %s %s\n", color.Green("DELETE"), desc)
		deleted++
	}

	// ── 1. Orphan clusters by label (root) ───────────────────────────────────
	clustersDeleted := false
	if o.ClusterLabel != "" {
		fmt.Println(color.Bold(fmt.Sprintf("==== orphan clusters (label %q) ====", o.ClusterLabel)))
		ids, err := client.ClustersWithLabel(ctx, o.ClusterLabel)
		if err != nil {
			return fmt.Errorf("list clusters: %w", err)
		}
		for _, id := range ids {
			del(fmt.Sprintf("/v4beta/lke/clusters/%d", id), fmt.Sprintf("cluster %d", id))
			clustersDeleted = true
		}
		if len(ids) == 0 {
			fmt.Println(color.Dim("  none matched"))
		}
		// Cluster delete is async; let it settle so the firewall safety guard
		// (which refuses while a live cluster still carries the label) passes.
		if confirm && clustersDeleted {
			fmt.Println(color.Dim("  (waiting 25s for cluster delete to settle)"))
			time.Sleep(25 * time.Second)
		}

		// ── 2. Orphan node firewall ──────────────────────────────────────────
		fmt.Println("\n" + color.Bold("==== orphan node firewall ===="))
		if err := ReapFirewalls(ctx, client, o, del); err != nil {
			return err
		}
	} else {
		fmt.Println(color.Bold("==== orphan clusters + firewall ====") + color.Dim(" — skipped (no --cluster-label)"))
	}

	// ── 3. NodeBalancers BEFORE VPCs (a parked NB blocks its VPC delete) ──────
	fmt.Println("\n" + color.Bold("==== orphan NodeBalancers (account-wide) ===="))
	if err := ReapNodeBalancers(ctx, client, o, del); err != nil {
		return err
	}

	// ── 4. VPCs (lke<id> cluster-gone, + <label>-vpc when --cluster-label) ────
	fmt.Println("\n" + color.Bold("==== orphan VPCs ===="))
	if err := ReapVPCs(ctx, client, o, del); err != nil {
		return err
	}

	// ── 5. Volumes (needs a scope: --region or --volume-ids) ──────────────────
	fmt.Println("\n" + color.Bold("==== orphan Volumes ===="))
	if o.Region == "" && o.VolumeIDs == "" {
		fmt.Println(color.Dim("  skipped — set --region and/or --volume-ids to scope the sweep (refusing an unscoped Volume delete)"))
	} else if err := ReapVolumes(ctx, client, o, del); err != nil {
		return err
	}

	// ── 6. Per-env minted Linode creds: obj-storage keys + in-cluster PAT ──────
	// These are ACCOUNT-scoped (no cluster tag), so they're keyed off the
	// deployment NAME, not cluster-liveness — a destroyed env's keys are orphaned.
	// Each bootstrap/rotation mints fresh ones under a stable per-env label, and a
	// leaked mint (failed run, failed drain) accretes toward the account's 100-key /
	// 100-PAT caps until a fresh mint 400s. Needs an explicit --env (never a blind
	// account-wide token/key delete).
	fmt.Println("\n" + color.Bold("==== orphan per-env Linode creds (obj-keys + in-cluster PAT) ===="))
	if o.Env == "" {
		fmt.Println(color.Dim("  skipped — set --env <deployment> to reap its minted keys + PAT"))
	} else {
		// The prefix namespaces the key labels this reaps. Read it from the spec —
		// an exact-label match under the wrong prefix would delete ANOTHER
		// instance's keys, which is the one mistake a reaper must never make.
		prefix, perr := objenc.LabelPrefixFor("reap")
		if perr != nil {
			return perr
		}
		if err := ReapEnvObjKeys(ctx, client, prefix, o.Env, del); err != nil {
			return err
		}
		if err := ReapEnvInclusterPAT(ctx, client, prefix, o.Env, del); err != nil {
			return err
		}
	}

	summary := fmt.Sprintf("summary: deleted=%d failed=%d", deleted, failed)
	if failed > 0 {
		summary = color.Red(summary)
	} else if deleted > 0 {
		summary = color.Green(summary)
	}
	fmt.Printf("\n%s\n", summary)
	if !confirm {
		fmt.Println(color.Dim("(dry-run — nothing was deleted; re-run with --yes)"))
	}
	if failed > 0 {
		return fmt.Errorf("%d delete(s) failed", failed)
	}
	return nil
}

func ReapFirewalls(ctx context.Context, client *linode.Client, o ReapOpts, del func(path, desc string)) error {
	// Candidate labels (account-unique, so each matches ≤1 firewall).
	var candidates []string
	if o.FwLabel != "" {
		candidates = []string{o.FwLabel}
	} else {
		candidates = []string{"platform-nodes-fw", truncate(o.ClusterLabel, 26) + "-nodes"}
	}
	// Safety: never delete a live cluster's firewall.
	if !o.Force {
		live, err := client.ClustersWithLabel(ctx, o.ClusterLabel)
		if err != nil {
			return fmt.Errorf("firewall safety check: %w", err)
		}
		if len(live) > 0 {
			fmt.Printf("  %s\n", color.Yellow(fmt.Sprintf("a live cluster still carries label %q — refusing (delete the cluster first, or --force)", o.ClusterLabel)))
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
		fmt.Printf("%s\n", color.Dim(fmt.Sprintf("  none matched (searched: %s)", strings.Join(candidates, ", "))))
	}
	return nil
}

func ReapNodeBalancers(ctx context.Context, client *linode.Client, o ReapOpts, del func(path, desc string)) error {
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
		if o.Region != "" && region != o.Region {
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
		fmt.Println(color.Dim("  none matched"))
	}
	return nil
}

func ReapVPCs(ctx context.Context, client *linode.Client, o ReapOpts, del func(path, desc string)) error {
	live, err := client.LiveClusterIDs(ctx)
	if err != nil {
		return fmt.Errorf("load live clusters: %w", err)
	}
	byoLabel := ""
	if o.ClusterLabel != "" {
		held, err := client.ClustersWithLabel(ctx, o.ClusterLabel)
		if err != nil {
			return err
		}
		if len(held) > 0 {
			fmt.Printf("  %s\n", color.Yellow(fmt.Sprintf("a live cluster still carries label %q — not targeting its %q VPC", o.ClusterLabel, o.ClusterLabel+"-vpc")))
		} else {
			byoLabel = o.ClusterLabel + "-vpc"
		}
	}
	vpcs, err := client.ListVPCs(ctx)
	if err != nil {
		return fmt.Errorf("list VPCs: %w", err)
	}
	matched := false
	for _, vpc := range vpcs {
		region := linode.MapString(vpc, "region")
		if o.Region != "" && region != o.Region {
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
		fmt.Println(color.Dim("  none matched"))
	}
	return nil
}

func ReapVolumes(ctx context.Context, client *linode.Client, o ReapOpts, del func(path, desc string)) error {
	idAllow := map[string]bool{}
	for _, id := range strings.Fields(o.VolumeIDs) {
		idAllow[id] = true
	}
	vols, err := client.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("list Volumes: %w", err)
	}
	matched, skipped := false, 0
	for _, v := range vols {
		id := linode.MapIDString(v)
		if !linode.VolumeIsCandidate(
			linode.VolumeLinodeIDNull(v), linode.MapString(v, "label"), linode.MapString(v, "region"),
			linode.MapTags(v), o.Region, idAllow, id, o.TagMustInclude,
			linode.VolumeLabelPrefixes(o.Env)...) {
			skipped++
			continue
		}
		del("/v4/volumes/"+id, fmt.Sprintf("volume %s (%s)", id, linode.MapString(v, "label")))
		matched = true
	}
	if !matched {
		// Never just "none matched": that reads identically to "nothing to do",
		// which is how a filter that excluded EVERYTHING stayed invisible for
		// weeks. Say what was skipped and what would widen the net.
		fmt.Printf("  none matched the filter (%d Volume(s) skipped)\n", skipped)
		if o.Env == "" {
			fmt.Println(color.Dim("  NOTE: LLZ's volume-labels reconciler renames bound volumes to"))
			fmt.Println(color.Dim("        <deployment>-<namespace>-<pvc>, which no longer start with \"pvc-\"."))
			fmt.Println(color.Dim("        Pass --env <deployment> (e.g. --env e2e) to include those."))
		}
	}
	return nil
}

// ── small helpers ────────────────────────────────────────────────────────────

// envObjKeyLabels are the Object Storage key labels the per-env reap targets —
// the obj-key entries credrotate.BuildRotationTable mints for a deployment. A test pins this
// in lockstep with credrotate.BuildRotationTable so a mint-label change can't silently orphan
// the reaper (the exact failure that let 76 keys pile up to the account cap).
func envObjKeyLabels(prefix, env string) []string {
	return clusterspec.ObjKeyLabels(prefix, env)
}

// ReapEnvObjKeys deletes the Object Storage keys minted for env — the loki +
// harbor-registry keys (labels <objLabelPrefix>-loki-<env> / <objLabelPrefix>-harbor-registry-<env>,
// per credrotate.BuildRotationTable). mint-bootstrap-objkeys and the in-cluster rotator each
// create a fresh key under the same stable label; a failed teardown or failed
// grace-window revoke leaks them, and the account caps at 100 keys (a fresh mint
// then 400s "reached your access key quota"). On a destroy the env is gone, so
// every key under those two labels is orphaned. Exact-label match — never another
// env's keys.
func ReapEnvObjKeys(ctx context.Context, client *linode.Client, prefix, env string, del func(path, desc string)) error {
	keys, err := client.ListObjectStorageKeys(ctx)
	if err != nil {
		return fmt.Errorf("list object-storage keys: %w", err)
	}
	want := map[string]bool{}
	for _, l := range envObjKeyLabels(prefix, env) {
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

// ReapEnvInclusterPAT deletes the narrow in-cluster PAT(s) minted for env (label
// llz-incluster-<objLabelPrefix>-<env>, per credrotate.InClusterPATLabel). mint-bootstrap-pat drains older
// siblings on each mint, but a failed drain / failed run leaks them toward the
// account's 100-PAT cap. Exact-label match — the broad token this sweep RUNS under
// carries a different label, so it is never self-revoked.
func ReapEnvInclusterPAT(ctx context.Context, client *linode.Client, prefix, env string, del func(path, desc string)) error {
	toks, err := client.ListProfileTokens(ctx)
	if err != nil {
		return fmt.Errorf("list profile tokens: %w", err)
	}
	label := credrotate.InClusterPATLabel(prefix, env)
	for _, t := range toks {
		if linode.MapString(t, "label") != label {
			continue
		}
		id := linode.MapUint(t, "id")
		del(fmt.Sprintf("/v4/profile/tokens/%d", id), fmt.Sprintf("in-cluster PAT %d (%s)", id, label))
	}
	return nil
}

// orAll renders an empty scope as "(all)".
//
// A COPY IN THIS PACKAGE HAS JUST BEEN DELETED IN FAVOUR OF THIS ONE. teardown.go
// carried its own, with a comment saying it was "a local copy of package main's
// helper" — and this is that helper, arriving with reap.go. Ten packages in this
// campaign kept a three-line copy rather than import one for it; this is the first
// time a copy and its original have ended up in the same package, and the copy is
// the one that goes.
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

// checkReapRegion rejects a `--region` that is not one of the account's Linode
// regions — most usefully, a DEPLOYMENT name.
//
// Best-effort in the same way checkRegion is (region_resolve.go): an
// unanswerable lookup returns nil rather than blocking a sweep that would have
// worked. But where a wrong value is KNOWN, this refuses instead of warning,
// because the failure it prevents is a false all-clear rather than an error —
// there is no later signal to catch it.
func checkReapRegion(region string) error {
	if region == "" {
		return nil // account-wide is a legitimate, and clearly-labelled, scope
	}
	ids, ok := instanceresolve.AccountRegions()
	if !ok {
		return nil
	}
	for _, id := range ids {
		if id == region {
			return nil
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--region %q is not a Linode region, so this sweep would match nothing and report a clean account.\n", region)
	b.WriteString("  `llz reap --region` takes a LINODE region (us-ord, us-sea) — unlike `llz ci …\n")
	b.WriteString("  --region`, which takes the DEPLOYMENT name. That is the usual mix-up here.\n")
	// Name the deployment's own region when the value looks like a deployment: it
	// turns the refusal into the command they meant to type.
	if lr := linodeRegionForDeployment(region); lr != "" {
		fmt.Fprintf(&b, "  %q is a deployment in this instance — its Linode region is %s:\n", region, lr)
		fmt.Fprintf(&b, "      %s\n", color.Cyan("llz reap --region "+lr))
	} else if near := instanceresolve.NearbyRegions(region, ids); len(near) > 0 {
		fmt.Fprintf(&b, "  Did you mean: %s\n", strings.Join(near, ", "))
	}
	b.WriteString("  List them with `linode-cli regions list`, or omit --region to sweep account-wide.")
	return fmt.Errorf("%s", b.String())
}

// linodeRegionForDeployment returns the Linode region of a deployment in this
// instance, or "" when the name is not a deployment (or there is no spec here —
// `llz reap` legitimately runs from anywhere).
func linodeRegionForDeployment(name string) string {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil || lz == nil {
		return ""
	}
	if e, ok := lz.Env(name); ok {
		return e.Cluster.Region
	}
	return ""
}
