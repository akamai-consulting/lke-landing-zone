package main

// ci_teardown.go implements the `llz ci teardown-*` family — native ports of
// the destroy-cluster job's inline Linode API steps in llz-terraform.yml
// (capture ids before destroy / force-delete stragglers / delete the VPC).
// The bash versions hand-rolled curl+jq against single-page list endpoints
// (page_size=500, silently truncating bigger accounts); these reuse the fully
// paginated internal/linode client the reap commands already drive. All three
// expect to run from the instance repo root (the terraform-iac-bootstrap/
// cluster paths are relative), like the workflow steps they replace.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/preflight"
	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
	"github.com/spf13/cobra"
)

// defaultClusterTFDir is where the destroy job's terraform working directory
// and tfvars live, relative to the instance repo root.
const defaultClusterTFDir = "terraform-iac-bootstrap/cluster"

// teardownClient is the slice of the Linode client the teardown steps use,
// seamed (like runner-acl's aclClient) so the capture/force-delete/VPC logic
// is unit-testable without the live API.
type teardownClient interface {
	ClustersWithLabel(ctx context.Context, label string) ([]uint64, error)
	ListNodePools(ctx context.Context, clusterID uint64) ([]map[string]any, error)
	ListVolumes(ctx context.Context) ([]map[string]any, error)
	ListFirewalls(ctx context.Context) ([]map[string]any, error)
	ListVPCs(ctx context.Context) ([]map[string]any, error)
	DeleteResourcePath(ctx context.Context, path string) error
}

var newTeardownClient = func(token string) teardownClient {
	return linode.NewClient(token, 60*time.Second)
}

func ciTeardownCaptureCmd() *cobra.Command {
	var region, tfDir string
	c := &cobra.Command{
		Use:   "teardown-capture",
		Short: "snapshot the cluster id + its attached pvc-* Volume ids before a destroy",
		Long: "Native port of the destroy job's 'Capture LKE cluster id + pvc Volume ids'\n" +
			"step. Both post-destroy sweeps must be scoped to THIS cluster, never the\n" +
			"region — co-located deployments share a Linode region and the block-storage\n" +
			"tag, so a region filter would never converge and could delete a live peer's\n" +
			"resources. NodeBalancers scope by the cluster's CCM tag (just the id), but\n" +
			"Volumes carry no cluster id and lose their linode_id once detached — so the\n" +
			"pvc-* Volumes attached to this cluster's nodes are snapshotted, by id, while\n" +
			"the cluster still exists. Writes LKE_CLUSTER_ID and CLUSTER_PVC_VOLUME_IDS\n" +
			"to $GITHUB_ENV. Read-only; reads LINODE_TOKEN (or LINODE_API_TOKEN).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCITeardownCapture(region, tfDir) },
	}
	c.Flags().StringVar(&region, "region", "", "tfvars prefix, e.g. primary (required)")
	c.Flags().StringVar(&tfDir, "tf-dir", defaultClusterTFDir, "cluster terraform root holding <region>.tfvars")
	return c
}

func ciTeardownForceDeleteCmd() *cobra.Command {
	var region, tfDir string
	c := &cobra.Command{
		Use:   "teardown-force-delete",
		Short: "force-delete the LKE cluster + node firewall stragglers after a destroy (--yes to delete)",
		Long: "Native port of the destroy job's 'Force-delete remaining Linode resources'\n" +
			"step: deletes the cluster by label if terraform couldn't (indeterminate\n" +
			"state / failed import), then the node-pool firewall — resolved by the exact\n" +
			"node_firewall_id terraform output first, then by the module-correct label\n" +
			"(firewall_label tfvars var, NOT a reconstructed \"<cluster>-nodes\" guess:\n" +
			"hardcoding that ignored var.firewall_label and leaked the firewall every\n" +
			"teardown). Delete failures warn rather than fail — this is always()-path\n" +
			"cleanup and the orphan reaper backstops it. Dry-run by default; --yes to\n" +
			"delete. Reads LINODE_TOKEN (or LINODE_API_TOKEN).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCITeardownForceDelete(gopts, region, tfDir)
		},
	}
	c.Flags().StringVar(&region, "region", "", "tfvars prefix, e.g. primary (required)")
	c.Flags().StringVar(&tfDir, "tf-dir", defaultClusterTFDir, "cluster terraform root holding <region>.tfvars")
	return c
}

func ciTeardownDeleteVPCCmd() *cobra.Command {
	var region, tfDir, clusterID string
	var attempts, retryDelay int
	var requireDeleted bool
	c := &cobra.Command{
		Use:   "teardown-delete-vpc",
		Short: "delete the cluster VPC, retrying the post-destroy in-use window (--yes to delete)",
		Long: "Native port of the destroy job's 'Delete cluster VPC' step. A VPC deletes\n" +
			"only once NOTHING is parked in its subnet — CCM NodeBalancers and node\n" +
			"Linodes both block it with a 409 — so this must run LAST, after the Volume\n" +
			"and NodeBalancer sweeps, and retries to ride out the async-release window.\n" +
			"Resolves the exact vpc_id terraform output first, then the\n" +
			"\"<cluster_label>-vpc\" label. Still-undeletable after the retries is a\n" +
			"warning by default; --require-deleted turns it into a NON-ZERO exit so a\n" +
			"destroy doesn't go green leaving an orphan VPC that blocks the next apply's\n" +
			"preflight. Dry-run by default; --yes to delete. Reads LINODE_TOKEN (or\n" +
			"LINODE_API_TOKEN).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCITeardownDeleteVPC(gopts, region, tfDir, clusterID, attempts, retryDelay, requireDeleted)
		},
	}
	c.Flags().StringVar(&region, "region", "", "tfvars prefix, e.g. primary (required)")
	c.Flags().StringVar(&tfDir, "tf-dir", defaultClusterTFDir, "cluster terraform root holding <region>.tfvars")
	c.Flags().StringVar(&clusterID, "cluster-id", "", "LKE cluster id — resolves the LKE-E auto VPC labeled lke<id> (defaults to the LKE_CLUSTER_ID env teardown-capture exports, so the workflow needs no new flag)")
	c.Flags().IntVar(&attempts, "attempts", 6, "delete attempts before giving up")
	c.Flags().IntVar(&retryDelay, "retry-delay", 20, "seconds between delete attempts")
	c.Flags().BoolVar(&requireDeleted, "require-deleted", false, "fail (non-zero) if the VPC is still undeletable after all attempts")
	return c
}

func ciAssertNoOrphansCmd() *cobra.Command {
	var region, volumeRegion, clusterID string
	var threshold, attempts, retryDelay int
	c := &cobra.Command{
		Use:   "assert-no-orphans",
		Short: "fail if orphaned Linode resources remain after a destroy (with retry)",
		Long: "Final destroy-job gate. Counts orphaned Volumes / NodeBalancers / VPCs with\n" +
			"the SAME account census `llz ci preflight` runs, and FAILS the job when the\n" +
			"count EXCEEDS --threshold — so a destroy can't go green leaving orphans that\n" +
			"stall the next apply's preflight. This backstops the scoped Volume/NB sweeps,\n" +
			"which no-op when the cluster was already gone at capture time (a re-run of a\n" +
			"partial destroy, or pre-existing orphans). Cluster-delete reaps NBs/VPCs\n" +
			"asynchronously and Volumes detach as the nodes tear down, so --attempts\n" +
			"re-counts (sleeping --retry-delay s) to ride out that settling window before\n" +
			"failing. Read-only — deletes NOTHING; clear survivors with `llz reap`.\n" +
			"NB/VPC are counted account-wide (cluster-id attributable); --volume-region\n" +
			"scopes the pvc-* Volume count (detached Volumes carry no cluster id, so an\n" +
			"account-wide count would flag other regions'/teams' Volumes reap can't clean).\n" +
			"--region scopes the NB/VPC census (empty = account-wide, matching preflight).\n" +
			"Reads LINODE_TOKEN (or LINODE_API_TOKEN).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIAssertNoOrphans(region, volumeRegion, clusterID, threshold, attempts, retryDelay)
		},
	}
	f := c.Flags()
	f.StringVar(&region, "region", "", "scope the NB/VPC orphan census to one region (empty = account-wide)")
	f.StringVar(&volumeRegion, "volume-region", "", "scope the pvc-* Volume orphan count to one region (empty = the --region value, or account-wide)")
	f.StringVar(&clusterID, "cluster-id", "", "the destroyed cluster's id: its OWN surviving NBs (lke_cluster.id) and VPC (lke<id>) fail the gate regardless of --threshold (defaults to the LKE_CLUSTER_ID env, so the workflow needs no new flag)")
	f.IntVar(&threshold, "threshold", 0, "only fail when the (other-account) orphan count EXCEEDS this")
	f.IntVar(&attempts, "attempts", 5, "re-count attempts before failing")
	f.IntVar(&retryDelay, "retry-delay", 30, "seconds between re-count attempts")
	return c
}

func runCIAssertNoOrphans(region, volumeRegion, clusterID string, threshold, attempts, retryDelay int) error {
	token, err := ciToken()
	if err != nil {
		return err
	}
	client := linode.NewClient(token, 60*time.Second)
	ctx := context.Background()
	if attempts < 1 {
		attempts = 1
	}
	volRegion := firstNonEmpty(volumeRegion, region)
	clusterID = firstNonEmpty(clusterID, os.Getenv("LKE_CLUSTER_ID"))

	// The destroyed cluster's OWN leftovers are unambiguously ours and must never
	// pass, regardless of --threshold (which only tolerates other-account orphans
	// the destroy can't clean). A surviving NB (lke_cluster.id) or VPC (lke<id>)
	// here means the scoped sweeps failed — fail loudly so the leak can't recur
	// silently the way it did before lke_cluster.id attribution.
	if clusterID != "" {
		nbs, verr := client.ListNodeBalancers(ctx)
		if verr != nil {
			return fmt.Errorf("list NodeBalancers: %w", verr)
		}
		ownNBs := 0
		for _, nb := range nbs {
			if nbBelongsToCluster(nb, clusterID) {
				ownNBs++
			}
		}
		vpcs, verr := client.ListVPCs(ctx)
		if verr != nil {
			return fmt.Errorf("list VPCs: %w", verr)
		}
		_, ownVPC := linode.FindIDByLabel(vpcs, "lke"+clusterID)
		ownVPCs := 0
		if ownVPC {
			ownVPCs = 1
		}
		if ownNBs > 0 || ownVPC {
			fmt.Fprintf(os.Stderr, "::error::cluster %s left its OWN orphans after destroy: %d NodeBalancer(s) + %d VPC. Clear them: LINODE_TOKEN=<token> llz reap --region %s --cluster-label <label> --yes\n",
				clusterID, ownNBs, ownVPCs, orAll(volRegion))
			return fmt.Errorf("assert-no-orphans: destroyed cluster %s left %d NodeBalancer(s) and %d VPC of its own", clusterID, ownNBs, ownVPCs)
		}
		fmt.Printf("cluster %s left no NodeBalancers or VPC of its own — scoped teardown is clean.\n", clusterID)
	}

	var scan orphanScan
	for attempt := 1; attempt <= attempts; attempt++ {
		if scan, err = scanOrphans(ctx, client, region, volRegion); err != nil {
			return err
		}
		fmt.Printf("orphan census (NB/VPC region: %s, Volume region: %s) [attempt %d/%d]: %d Volume(s) [gone-cluster], %d NodeBalancer(s), %d VPC(s) — %d total (threshold %d); %d untagged detached Volume(s) not gated\n",
			orAll(region), orAll(volRegion), attempt, attempts, scan.vol.orphan, scan.nb.orphan, scan.vpc.orphan, scan.orphans(), threshold, scan.vol.untagged)
		if !preflight.OrphansExceedThreshold(scan.orphans(), threshold) {
			fmt.Println("no orphaned resources above threshold — destroy is clean.")
			return nil
		}
		if attempt < attempts {
			fmt.Printf("orphans still present — re-checking in %ds (cluster-delete reaps NBs/VPCs and detaches Volumes asynchronously)...\n", retryDelay)
			time.Sleep(time.Duration(retryDelay) * time.Second)
		}
	}
	fmt.Fprintf(os.Stderr, "::error::%d orphaned Linode resource(s) remain after the destroy (threshold %d): %d Volume(s), %d NodeBalancer(s), %d VPC(s). These count against the account's active-services quota and will stall the next apply's preflight. Clear them: LINODE_TOKEN=<token> llz reap --region %s --yes\n",
		scan.orphans(), threshold, scan.vol.orphan, scan.nb.orphan, scan.vpc.orphan, orAll(volRegion))
	return fmt.Errorf("assert-no-orphans: %d orphaned resource(s) over threshold %d after %d attempt(s)", scan.orphans(), threshold, attempts)
}

// teardownLabels reads <tf-dir>/<region>.tfvars (falling back to the .example,
// like tf-import) and derives the cluster resource labels.
func teardownLabels(region, tfDir string) (tf.Labels, error) {
	if region == "" {
		return tf.Labels{}, fmt.Errorf("--region is required (the tfvars prefix, e.g. primary)")
	}
	varFile := tfDir + "/" + region + ".tfvars"
	if _, err := os.Stat(varFile); err != nil {
		varFile = tfDir + "/" + region + ".tfvars.example"
	}
	content, err := os.ReadFile(varFile)
	if err != nil {
		return tf.Labels{}, fmt.Errorf("read %s: %w", varFile, err)
	}
	labels := tf.DeriveLabels(tf.ParseTFVars(string(content)))
	if labels.Cluster == "" {
		return tf.Labels{}, fmt.Errorf("%s has no cluster_label", varFile)
	}
	return labels, nil
}

// tfOutputRaw returns `terraform -chdir=<dir> output -raw <name>`, "" when the
// output is absent / state empty (the bash `2>/dev/null || true`).
func tfOutputRaw(dir, name string) string {
	out, err := execOutput("terraform", "-chdir="+dir, "output", "-raw", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// numericOrEmpty mirrors the bash `case in ”|*[!0-9]*)` guard: terraform
// output noise ("Warning: No outputs found") must not be mistaken for an id.
func numericOrEmpty(s string) string {
	if _, err := strconv.ParseUint(s, 10, 64); err != nil {
		return ""
	}
	return s
}

func runCITeardownCapture(region, tfDir string) error {
	labels, err := teardownLabels(region, tfDir)
	if err != nil {
		return err
	}
	token, err := ciToken()
	if err != nil {
		return err
	}
	client := newTeardownClient(token)
	ctx := context.Background()

	clusterID := ""
	if ids, err := client.ClustersWithLabel(ctx, labels.Cluster); err != nil {
		return fmt.Errorf("list clusters: %w", err)
	} else if len(ids) > 0 {
		clusterID = strconv.FormatUint(ids[0], 10)
	}

	// Snapshot the pvc-* Volume ids attached to this cluster's nodes. If the
	// cluster is already gone there's nothing to track — any attached pvc-*
	// Volumes in the region belong to a live peer and are correctly ignored.
	volIDs := ""
	if clusterID != "" {
		cNum, _ := strconv.ParseUint(clusterID, 10, 64)
		pools, err := client.ListNodePools(ctx, cNum)
		if err != nil {
			return fmt.Errorf("list node pools of cluster %s: %w", clusterID, err)
		}
		nodeIDs := map[uint64]bool{}
		for _, pool := range pools {
			nodes, _ := pool["nodes"].([]any)
			for _, n := range nodes {
				if nm, ok := n.(map[string]any); ok {
					if id := linode.MapUint(nm, "instance_id"); id != 0 {
						nodeIDs[id] = true
					}
				}
			}
		}
		vols, err := client.ListVolumes(ctx)
		if err != nil {
			return fmt.Errorf("list Volumes: %w", err)
		}
		var tracked []string
		for _, v := range vols {
			if !strings.HasPrefix(linode.MapString(v, "label"), "pvc-") {
				continue
			}
			if linode.VolumeLinodeIDNull(v) || !nodeIDs[linode.MapUint(v, "linode_id")] {
				continue
			}
			tracked = append(tracked, linode.MapIDString(v))
		}
		volIDs = strings.Join(tracked, " ")
	}

	fmt.Printf("captured cluster %q id=%q; tracked pvc Volume ids: %q\n",
		labels.Cluster, clusterID, firstNonEmpty(volIDs, "<none>"))
	return appendGHAFile("GITHUB_ENV",
		"LKE_CLUSTER_ID="+clusterID,
		"CLUSTER_PVC_VOLUME_IDS="+volIDs)
}

func runCITeardownForceDelete(g globalOpts, region, tfDir string) error {
	labels, err := teardownLabels(region, tfDir)
	if err != nil {
		return err
	}
	token, err := ciToken()
	if err != nil {
		return err
	}
	client := newTeardownClient(token)
	ctx := context.Background()
	confirm := g.yes && !g.dryRun
	if !confirm {
		fmt.Println("DRY-RUN — nothing will be deleted. Re-run with --yes to delete.")
	}
	// Warn-don't-fail delete: this runs on the always() cleanup path where the
	// orphan reaper backstops anything left behind (the bash `|| true`).
	del := func(path, desc string) {
		if !confirm {
			fmt.Printf("  would DELETE %s\n", desc)
			return
		}
		if err := client.DeleteResourcePath(ctx, path); err != nil {
			fmt.Fprintf(os.Stderr, "::warning::DELETE %s failed (%v) — the orphan reaper will catch it.\n", desc, err)
			return
		}
		fmt.Printf("  DELETE %s (deletion is asynchronous)\n", desc)
	}

	// ── LKE cluster, if terraform couldn't reach or import it ──
	ids, err := client.ClustersWithLabel(ctx, labels.Cluster)
	if err != nil {
		return fmt.Errorf("list clusters: %w", err)
	}
	if len(ids) == 0 {
		fmt.Printf("Cluster %q not found — already deleted.\n", labels.Cluster)
	}
	for _, id := range ids {
		fmt.Printf("Cluster %q (id=%d) still exists — deleting via API...\n", labels.Cluster, id)
		del(fmt.Sprintf("/v4beta/lke/clusters/%d", id), fmt.Sprintf("cluster %d", id))
	}

	// ── Node firewall: exact id output first, then the module-correct label ──
	fwID := numericOrEmpty(tfOutputRaw(tfDir, "node_firewall_id"))
	if fwID == "" {
		label := firstNonEmpty(tfOutputRaw(tfDir, "node_firewall_label"), labels.Firewall)
		fws, err := client.ListFirewalls(ctx)
		if err != nil {
			return fmt.Errorf("list firewalls: %w", err)
		}
		if id, ok := linode.FindIDByLabel(fws, label); ok {
			fwID = strconv.FormatUint(id, 10)
		}
	}
	if fwID == "" {
		fmt.Println("Node firewall not found — already deleted.")
		return nil
	}
	fmt.Printf("Node firewall (id=%s) still exists — deleting via API...\n", fwID)
	del("/v4/networking/firewalls/"+fwID, "firewall "+fwID)
	return nil
}

func runCITeardownDeleteVPC(g globalOpts, region, tfDir, clusterID string, attempts, retryDelay int, requireDeleted bool) error {
	labels, err := teardownLabels(region, tfDir)
	if err != nil {
		return err
	}
	token, err := ciToken()
	if err != nil {
		return err
	}
	client := newTeardownClient(token)
	ctx := context.Background()
	clusterID = firstNonEmpty(clusterID, os.Getenv("LKE_CLUSTER_ID"))

	// Resolution order: the exact vpc_id terraform output, then the BYO
	// "<cluster_label>-vpc" label, then — for LKE-E, which auto-creates a VPC
	// labeled lke<cluster_id> that no terraform output or BYO label names — the
	// captured cluster id. Missing that last path leaked the lke<id> VPC every
	// teardown (the vpc_id output is empty once the cluster state is destroyed).
	vpcID := numericOrEmpty(tfOutputRaw(tfDir, "vpc_id"))
	if vpcID == "" {
		vpcs, err := client.ListVPCs(ctx)
		if err != nil {
			return fmt.Errorf("list VPCs: %w", err)
		}
		if id, ok := linode.FindIDByLabel(vpcs, labels.VPC); ok {
			vpcID = strconv.FormatUint(id, 10)
		} else if clusterID != "" {
			if id, ok := linode.FindIDByLabel(vpcs, "lke"+clusterID); ok {
				vpcID = strconv.FormatUint(id, 10)
			}
		}
	}
	if vpcID == "" {
		fmt.Println("VPC not found — already deleted.")
		return nil
	}
	if !(g.yes && !g.dryRun) {
		fmt.Printf("DRY-RUN — would DELETE vpc %s. Re-run with --yes to delete.\n", vpcID)
		return nil
	}

	fmt.Printf("Deleting VPC %s (retrying the 409/in-use window while the cluster + NodeBalancers finish releasing the subnet)...\n", vpcID)
	for attempt := 1; attempt <= attempts; attempt++ {
		// DeleteResourcePath treats 2xx AND 404 (already gone) as success.
		if err := client.DeleteResourcePath(ctx, "/v4/vpcs/"+vpcID); err == nil {
			fmt.Printf("VPC %s deleted.\n", vpcID)
			return nil
		} else if attempt < attempts {
			fmt.Printf("VPC %s delete failed (attempt %d/%d): %v — still in use; retrying in %ds...\n",
				vpcID, attempt, attempts, err, retryDelay)
			time.Sleep(time.Duration(retryDelay) * time.Second)
		}
	}
	fmt.Fprintf(os.Stderr, "::warning::VPC %s still not deletable after %d attempts — a straggler NodeBalancer or other device is likely still parked in its subnet. The orphan reaper ('llz reap --region <r>', or 'make reap-orphans') will catch it; run it with the e2e LINODE_TOKEN if VPC quota is tight.\n", vpcID, attempts)
	if requireDeleted {
		return fmt.Errorf("teardown-delete-vpc: VPC %s still not deletable after %d attempt(s) — orphan VPC remains; failing the destroy so it doesn't block the next apply's preflight", vpcID, attempts)
	}
	return nil
}
