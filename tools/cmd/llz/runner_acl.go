package main

// runner_acl.go implements `llz ci runner-acl <open|revoke>` — the native port of
// the lke-runner-acl composite action. It adds (open) or removes (revoke) THIS
// runner's public egress IP to/from an LKE-E cluster's control-plane ACL so
// kubectl against the API server is permitted for the duration of a job.
//
// The old static-ACL design (github_runner_ipv4_cidrs) assumed a pre-known runner
// range — true for self-hosted runners, FALSE for github.com-hosted runners whose
// egress IP is dynamic per job. open detects the egress IP at run time, adds it,
// and records what changed in a per-region state file so the paired revoke (run
// with `if: always()`) is self-describing and idempotent.
//
// The fiddly read-modify-write of the ACL address set lives, tested, in
// internal/linode (ControlPlaneACL.WithIP/WithoutIP); this file is the
// orchestration: token + cluster resolution, IP detection, the state file, and
// the open-path PUT retry that absorbs a racing writer.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
	"github.com/spf13/cobra"
)

// clusterLister is the cluster-resolution slice shared by runner-acl and
// fetch-kubeconfig; aclClient and the fetch-kubeconfig client both satisfy it.
type clusterLister interface {
	ListClusters(ctx context.Context) ([]map[string]any, error)
}

// aclClient is the slice of the Linode client runner-acl needs — injected so the
// orchestration is testable against a fake. *linode.Client satisfies it.
type aclClient interface {
	clusterLister
	GetControlPlaneACL(ctx context.Context, clusterID uint64) (linode.ControlPlaneACL, error)
	PutControlPlaneACL(ctx context.Context, clusterID uint64, acl linode.ControlPlaneACL) error
}

// clusterRef is the resolve-this-cluster input shared by the CI commands: an
// explicit numeric ID, else a label (+ Linode region), else the cluster_label /
// region read from <tfvarsDir>/<region>.tfvars.
type clusterRef struct {
	region       string // deployment/env key — finds the tfvars
	clusterID    string
	clusterLabel string
	linodeRegion string
	tfvarsDir    string
}

// Seams (overridden in tests).
var (
	newACLClient     = func(token string) aclClient { return linode.NewClient(token, 30*time.Second) }
	detectEgressIPFn = detectEgressIP
	aclRetryDelay    = 3 * time.Second
)

type runnerACLOpts struct {
	region        string // deployment/env key — names the state file, finds the tfvars
	clusterID     string // explicit numeric LKE cluster ID (skips resolution)
	clusterLabel  string
	linodeRegion  string // Linode datacenter region (e.g. us-ord) — disambiguates
	ip            string // egress IP override; auto-detected when empty
	tfvarsDir     string
	failOnMissing bool
	configMap     bool // also lease/release the IP in the firewall-runner-acl ConfigMap (needs KUBECONFIG)
}

func ciRunnerACLCmd() *cobra.Command {
	var o runnerACLOpts
	c := &cobra.Command{
		Use:   "runner-acl <open|revoke>",
		Short: "add/remove this runner's egress IP in the LKE-E control-plane ACL",
		Long: "Native port of the lke-runner-acl composite action. open detects this\n" +
			"runner's public egress IP and adds it to the cluster's control-plane ACL so\n" +
			"kubectl is permitted; revoke removes it again (run with `if: always()`). open\n" +
			"records what it changed in a per-region state file under RUNNER_TEMP so revoke\n" +
			"is self-describing and idempotent — a no-op when open made no change (ACL\n" +
			"disabled, or the IP was already present). Reads LINODE_API_TOKEN (or\n" +
			"LINODE_TOKEN); an empty token no-ops with a warning so the failure surfaces\n" +
			"later on kubectl with a clear ACL message. The cluster is resolved from\n" +
			"--cluster-id, else --cluster-label (+ --linode-region), else cluster_label /\n" +
			"region read from <tfvars-dir>/<region>.tfvars.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return runRunnerACL(args[0], o) },
	}
	f := c.Flags()
	f.StringVar(&o.region, "region", "", "deployment/env key (names the state file; finds <region>.tfvars)")
	f.StringVar(&o.clusterID, "cluster-id", "", "explicit LKE cluster numeric ID (skips label resolution)")
	f.StringVar(&o.clusterLabel, "cluster-label", "", "LKE cluster label to resolve by")
	f.StringVar(&o.linodeRegion, "linode-region", "", "Linode datacenter region (e.g. us-ord) to disambiguate")
	f.StringVar(&o.ip, "ip", "", "egress IP to add/remove (default: auto-detect)")
	f.StringVar(&o.tfvarsDir, "tfvars-dir", "terraform-iac-bootstrap/cluster", "dir holding <region>.tfvars")
	f.BoolVar(&o.failOnMissing, "fail-on-missing", true, "open: fail if the cluster can't be resolved")
	f.BoolVar(&o.configMap, "runner-configmap", false, "also lease/release the IP in the firewall-runner-acl ConfigMap so the internal-CIDR firewall controller preserves it (requires KUBECONFIG)")
	return c
}

// runnerACLState is the per-region record open writes so revoke is self-describing.
// cluster_id is a string to match the lke-runner-acl action's state file exactly,
// so an in-flight open/revoke pair survives the action→llz cutover either way.
type runnerACLState struct {
	ClusterID string `json:"cluster_id"`
	IP        string `json:"ip"`
	Modified  bool   `json:"modified"`
}

func runRunnerACL(mode string, o runnerACLOpts) error {
	if mode != "open" && mode != "revoke" {
		return fmt.Errorf("mode must be 'open' or 'revoke' (got %q)", mode)
	}
	token := firstNonEmpty(os.Getenv("LINODE_API_TOKEN"), os.Getenv("LINODE_TOKEN"))
	if token == "" {
		fmt.Fprintf(os.Stderr, "::warning::runner-acl(%s): no LINODE_API_TOKEN/LINODE_TOKEN — skipping. "+
			"kubectl will fail later with an ACL error if this runner IP is not already allowed.\n", mode)
		return nil
	}
	client := newACLClient(token)
	ctx := context.Background()

	if mode == "revoke" {
		return runnerACLRevoke(ctx, client, o)
	}
	return runnerACLOpen(ctx, client, o)
}

func runnerACLOpen(ctx context.Context, client aclClient, o runnerACLOpts) error {
	cid, err := resolveClusterID(ctx, client, clusterRef{
		region: o.region, clusterID: o.clusterID, clusterLabel: o.clusterLabel,
		linodeRegion: o.linodeRegion, tfvarsDir: o.tfvarsDir,
	})
	if err != nil {
		if !o.failOnMissing {
			fmt.Printf("runner-acl(open): cluster not resolvable and --fail-on-missing=false — no-op (nothing to allow).\n")
			return nil
		}
		return err
	}

	ip := o.ip
	if ip == "" {
		if ip, err = detectEgressIPFn(); err != nil {
			return fmt.Errorf("could not detect runner egress IP: %w", err)
		}
	}
	fmt.Printf("runner-acl(open): runner egress IP %s, cluster %d.\n", ip, cid)

	acl, err := client.GetControlPlaneACL(ctx, cid)
	if err != nil {
		return err
	}
	if !acl.Enabled {
		fmt.Printf("runner-acl(open): control-plane ACL is disabled (open to all) — no change needed.\n")
		return writeRunnerACLState(o.region, runnerACLState{ClusterID: strconv.FormatUint(cid, 10), IP: ip, Modified: false})
	}
	if acl.ContainsIP(ip) {
		fmt.Printf("runner-acl(open): %s already in cluster %d ACL — no change.\n", ip, cid)
		// Still (re)lease it: the IP may be present only because a prior reconcile
		// preserved an existing lease, which must be refreshed to keep it.
		if o.configMap {
			registerRunnerACLIP(ip)
		}
		return writeRunnerACLState(o.region, runnerACLState{ClusterID: strconv.FormatUint(cid, 10), IP: ip, Modified: false})
	}

	// Read-modify-write with a small retry to absorb a racing writer: on a failed
	// PUT, re-read the ACL and re-add before retrying.
	var putErr error
	for attempt := 1; attempt <= 3; attempt++ {
		next, _ := acl.WithIP(ip)
		if putErr = client.PutControlPlaneACL(ctx, cid, next); putErr == nil {
			fmt.Printf("runner-acl(open): added %s to cluster %d control-plane ACL.\n", ip, cid)
			// Lease it so the internal-CIDR firewall controller's next reconcile
			// preserves the IP instead of replacing it out from under a long-running
			// kubectl job.
			if o.configMap {
				registerRunnerACLIP(ip)
			}
			return writeRunnerACLState(o.region, runnerACLState{ClusterID: strconv.FormatUint(cid, 10), IP: ip, Modified: true})
		}
		fmt.Fprintf(os.Stderr, "::warning::runner-acl(open): PUT attempt %d failed (%v); re-reading and retrying.\n", attempt, putErr)
		time.Sleep(aclRetryDelay)
		if reread, gerr := client.GetControlPlaneACL(ctx, cid); gerr == nil {
			acl = reread
		}
	}
	return fmt.Errorf("failed to add %s to cluster %d control-plane ACL after retries: %w", ip, cid, putErr)
}

func runnerACLRevoke(ctx context.Context, client aclClient, o runnerACLOpts) error {
	st, ok, err := readRunnerACLState(o.region)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("runner-acl(revoke): no state file — no-op.\n")
		return nil
	}
	// Release the ConfigMap lease first, while the apiserver is still reachable
	// (the Linode-API ACL removal below cuts that access). open leases the IP
	// even when it made no ACL change, so release regardless of Modified.
	if o.configMap && st.IP != "" {
		deregisterRunnerACLIP(st.IP)
	}
	if !st.Modified || st.IP == "" || st.ClusterID == "" {
		fmt.Printf("runner-acl(revoke): nothing recorded as opened — no-op.\n")
		return removeRunnerACLState(o.region)
	}
	cid, err := strconv.ParseUint(st.ClusterID, 10, 64)
	if err != nil {
		return fmt.Errorf("state file has a non-numeric cluster_id %q: %w", st.ClusterID, err)
	}

	acl, err := client.GetControlPlaneACL(ctx, cid)
	if err != nil {
		// A read failure must not strand the state file — surface a warning and
		// leave it so a later revoke can retry; matches the action's tolerance.
		fmt.Fprintf(os.Stderr, "::warning::runner-acl(revoke): GET ACL for cluster %d failed (%v); %s may persist — prune manually.\n", cid, err, st.IP)
		return nil
	}
	next, changed := acl.WithoutIP(st.IP)
	if !changed {
		fmt.Printf("runner-acl(revoke): %s already absent from cluster %d ACL — no change.\n", st.IP, cid)
		return removeRunnerACLState(o.region)
	}
	if err := client.PutControlPlaneACL(ctx, cid, next); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::runner-acl(revoke): PUT ACL for cluster %d failed (%v); %s may still be allowed — prune manually.\n", cid, err, st.IP)
		return nil
	}
	fmt.Printf("runner-acl(revoke): removed %s from cluster %d control-plane ACL.\n", st.IP, cid)
	return removeRunnerACLState(o.region)
}

// resolveClusterID returns the target cluster's numeric ID from r.clusterID, else
// r.clusterLabel (+ r.linodeRegion), else cluster_label/region read from
// <tfvarsDir>/<region>.tfvars — mirroring the action's resolve_cluster_id.
func resolveClusterID(ctx context.Context, lister clusterLister, r clusterRef) (uint64, error) {
	if r.clusterID != "" {
		id, err := strconv.ParseUint(r.clusterID, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("--cluster-id %q is not numeric: %w", r.clusterID, err)
		}
		return id, nil
	}
	label, lregion := r.clusterLabel, r.linodeRegion
	if (label == "" || lregion == "") && r.region != "" {
		path := filepath.Join(r.tfvarsDir, r.region+".tfvars")
		if content, rerr := os.ReadFile(path); rerr == nil {
			v := tf.ParseTFVars(string(content))
			if label == "" {
				label = v.ClusterLabel
			}
			if lregion == "" {
				lregion = v.Region
			}
		}
	}
	if label == "" {
		return 0, fmt.Errorf("cannot determine cluster label (no --cluster-id, no --cluster-label, no cluster_label in %s/%s.tfvars)", r.tfvarsDir, r.region)
	}
	clusters, err := lister.ListClusters(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing LKE clusters: %w", err)
	}
	ids := linode.MatchClusterIDs(clusters, label, lregion)
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return 0, fmt.Errorf("no LKE cluster matched label=%q linode-region=%q (env=%q); pass --cluster-id or --linode-region", label, lregion, r.region)
	default:
		return 0, fmt.Errorf("%d clusters matched label=%q linode-region=%q (ambiguous); pass --cluster-id explicitly: %v", len(ids), label, lregion, ids)
	}
}

// ── egress IP detection ──────────────────────────────────────────────────────

func detectEgressIP() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	for _, u := range []string{"https://api.ipify.org", "https://checkip.amazonaws.com", "https://ifconfig.me/ip"} {
		resp, err := client.Get(u)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		ip := strings.TrimSpace(string(b))
		if p := net.ParseIP(ip); p != nil && p.To4() != nil {
			return ip, nil
		}
	}
	return "", fmt.Errorf("none of the egress-IP probes returned an IPv4 address")
}

// ── per-region state file ────────────────────────────────────────────────────

func runnerACLStatePath(region string) string {
	dir := os.Getenv("RUNNER_TEMP")
	if dir == "" {
		dir = os.TempDir()
	}
	key := region
	if key == "" {
		key = "default"
	}
	return filepath.Join(dir, ".lke-runner-acl-"+key+".json")
}

func writeRunnerACLState(region string, st runnerACLState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(runnerACLStatePath(region), b, 0o600)
}

func readRunnerACLState(region string) (runnerACLState, bool, error) {
	b, err := os.ReadFile(runnerACLStatePath(region))
	if os.IsNotExist(err) {
		return runnerACLState{}, false, nil
	}
	if err != nil {
		return runnerACLState{}, false, err
	}
	var st runnerACLState
	if err := json.Unmarshal(b, &st); err != nil {
		return runnerACLState{}, false, fmt.Errorf("parsing runner-acl state file: %w", err)
	}
	return st, true, nil
}

func removeRunnerACLState(region string) error {
	if err := os.Remove(runnerACLStatePath(region)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
