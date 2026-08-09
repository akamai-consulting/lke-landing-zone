package clusteraccess

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
// the read-modify-write retry that absorbs a racing writer.
//
// CONCURRENCY: the control-plane ACL is a single PUT-replaces-the-whole-list
// resource with no server-side compare-and-swap. Two jobs opening/revoking THIS
// cluster's ACL in parallel (e.g. the converge gate running alongside the Harbor
// provisioning job) each do GET→modify→PUT; two *successful* PUTs silently
// last-writer-wins, so one runner's IP can be dropped (its kubectl is then
// refused) or a revoke can be undone (a dead runner IP left allowed). The PUT
// here is therefore VERIFY-AFTER-WRITE: re-read the ACL and confirm our mutation
// actually persisted; if a racer clobbered it, re-read their current list and
// retry (with jitter, to break lockstep). This converges for the handful of
// concurrent writers a bootstrap ever has without needing server-side CAS.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
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
	PutControlPlaneACL(ctx context.Context, clusterID uint64, acl linode.ControlPlaneACL) (string, error)
}

// ClusterRef is the resolve-this-cluster input shared by the CI commands: an
// explicit numeric ID, else a label (+ Linode region), else the cluster_label /
// region read from <tfvarsDir>/<region>.tfvars.
type ClusterRef struct {
	Region       string // deployment/env key — finds the tfvars
	ClusterID    string
	ClusterLabel string
	LinodeRegion string
	TfvarsDir    string
}

// Seams (overridden in tests).
var (
	newACLClient   = func(token string) aclClient { return linode.NewClient(token, 30*time.Second) }
	aclRetryDelay  = 3 * time.Second
	aclMaxAttempts = 4
	// aclSleep backs off between ACL read-modify-write retries. The jitter
	// (up to +50% of base) breaks lockstep between two runners retrying in
	// parallel so they don't keep clobbering each other on the same cadence.
	// base <= 0 (tests) sleeps not at all and never touches the RNG.
	aclSleep = func(base time.Duration) {
		if base <= 0 {
			return
		}
		time.Sleep(base + time.Duration(rand.Int63n(int64(base/2)+1)))
	}
)

type ACLOpts struct {
	Region        string // deployment/env key — names the state file, finds the tfvars
	ClusterID     string // explicit numeric LKE cluster ID (skips resolution)
	ClusterLabel  string
	LinodeRegion  string // Linode datacenter region (e.g. us-ord) — disambiguates
	Ip            string // egress IP override; auto-detected when empty
	TfvarsDir     string
	FailOnMissing bool
	ConfigMap     bool // also lease/release the IP in the firewall-runner-acl ConfigMap (needs KUBECONFIG)
	// dryRun mirrors the ROOT --dry-run flag. It is read in RunE rather than
	// declared here as a local flag so `llz --dry-run ci runner-acl ...` and
	// `llz ci runner-acl --dry-run ...` behave identically.
	DryRun bool
}

// runnerACLState is the per-region record open writes so revoke is self-describing.
// cluster_id is a string to match the lke-runner-acl action's state file exactly,
// so an in-flight open/revoke pair survives the action→llz cutover either way.
type runnerACLState struct {
	ClusterID string `json:"cluster_id"`
	IP        string `json:"ip"`
	Modified  bool   `json:"modified"`
}

func RunACL(d Deps, mode string, o ACLOpts) error {
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

	if o.DryRun {
		// A SEPARATE path, not a flag threaded through the write path. The open /
		// revoke flows do more than the one PUT — they verify-after-write, wait for
		// the revision to be enforced, optionally lease the IP into a ConfigMap, and
		// write a state file that the paired revoke later acts on. Making all of that
		// pretend convincingly means several places that must each remember to check
		// a bool, and the cost of missing one is a mutation from a flag that promised
		// none. This branch can only GET, so it cannot mutate by construction.
		return runnerACLDryRun(d, ctx, client, mode, o)
	}
	if mode == "revoke" {
		return runnerACLRevoke(ctx, client, o)
	}
	return runnerACLOpen(d, ctx, client, o)
}

// runnerACLDryRun reports what open/revoke WOULD change and returns. It performs
// reads only — no PUT, no ConfigMap lease, no state file — so `--dry-run` is
// honest about leaving the cluster and the runner's own state untouched.
func runnerACLDryRun(d Deps, ctx context.Context, client aclClient, mode string, o ACLOpts) error {
	cid, err := resolveClusterID(ctx, client, ClusterRef{
		Region: o.Region, ClusterID: o.ClusterID, ClusterLabel: o.ClusterLabel,
		LinodeRegion: o.LinodeRegion, TfvarsDir: o.TfvarsDir,
	})
	if err != nil {
		if mode == "open" && !o.FailOnMissing {
			fmt.Printf("dry-run runner-acl(open): cluster not resolvable and --fail-on-missing=false — would no-op.\n")
			return nil
		}
		return err
	}

	ip := o.Ip
	if ip == "" {
		if ip, err = detectEgressIP(); err != nil {
			return fmt.Errorf("could not detect runner egress IP: %w", err)
		}
	}

	acl, err := client.GetControlPlaneACL(ctx, cid)
	if err != nil {
		return fmt.Errorf("read control-plane ACL for cluster %d: %w", cid, err)
	}
	fmt.Printf("dry-run runner-acl(%s): cluster %d, runner IP %s.\n", mode, cid, ip)
	fmt.Printf("dry-run: current ACL enabled=%t ipv4=%v\n", acl.Enabled, acl.IPv4)

	if !acl.Enabled {
		fmt.Printf("dry-run: ACL is disabled (open to all) — would make no change.\n")
		return nil
	}
	if mode == "open" {
		if acl.ContainsIP(ip) {
			fmt.Printf("dry-run: %s already present — would make no change.\n", ip)
			return nil
		}
		next, _ := acl.WithIP(ip)
		fmt.Printf("dry-run: WOULD PUT ipv4=%v (adding %s); no request sent.\n", next.IPv4, ip)
		if o.ConfigMap {
			fmt.Printf("dry-run: would also lease %s in the firewall-runner-acl ConfigMap.\n", ip)
		}
		return nil
	}
	if !acl.ContainsIP(ip) {
		fmt.Printf("dry-run: %s not present — would make no change.\n", ip)
		return nil
	}
	next, _ := acl.WithoutIP(ip)
	fmt.Printf("dry-run: WOULD PUT ipv4=%v (removing %s); no request sent.\n", next.IPv4, ip)
	return nil
}

func runnerACLOpen(d Deps, ctx context.Context, client aclClient, o ACLOpts) error {
	cid, err := resolveClusterID(ctx, client, ClusterRef{
		Region: o.Region, ClusterID: o.ClusterID, ClusterLabel: o.ClusterLabel,
		LinodeRegion: o.LinodeRegion, TfvarsDir: o.TfvarsDir,
	})
	if err != nil {
		if !o.FailOnMissing {
			fmt.Printf("runner-acl(open): cluster not resolvable and --fail-on-missing=false — no-op (nothing to allow).\n")
			return nil
		}
		return err
	}

	ip := o.Ip
	if ip == "" {
		if ip, err = detectEgressIP(); err != nil {
			return fmt.Errorf("could not detect runner egress IP: %w", err)
		}
	}
	fmt.Printf("runner-acl(open): runner egress IP %s, cluster %d.\n", ip, cid)

	// Read-modify-write with verify-after-write (see the CONCURRENCY note at the
	// top of the file): each attempt re-reads the CURRENT ACL — so a racing
	// writer's additions are preserved — adds our IP, PUTs, then re-reads to
	// confirm our IP actually persisted before declaring success.
	var lastErr error
	for attempt := 1; attempt <= aclMaxAttempts; attempt++ {
		acl, err := client.GetControlPlaneACL(ctx, cid)
		if err != nil {
			lastErr = err
			fmt.Fprintf(os.Stderr, "::warning::runner-acl(open): GET attempt %d failed (%v); retrying.\n", attempt, err)
			aclSleep(aclRetryDelay)
			continue
		}
		if !acl.Enabled {
			fmt.Printf("runner-acl(open): control-plane ACL is disabled (open to all) — no change needed.\n")
			return writeRunnerACLState(o.Region, runnerACLState{ClusterID: strconv.FormatUint(cid, 10), IP: ip, Modified: false})
		}
		if acl.ContainsIP(ip) {
			// Present already — either a prior reconcile preserved a lease or a
			// concurrent open added us. Either way it's the desired end-state, and
			// THIS invocation made no change (Modified=false), so revoke won't
			// remove an IP this job didn't add.
			fmt.Printf("runner-acl(open): %s present in cluster %d ACL — no change.\n", ip, cid)
			// Still (re)lease it: the IP may be present only because a prior reconcile
			// preserved an existing lease, which must be refreshed to keep it.
			if o.ConfigMap {
				if lerr := registerRunnerACLIP(ip, reassertRunnerACL(ctx, client, cid, ip)); lerr != nil {
					// State is written first so the paired revoke still cleans up.
					_ = writeRunnerACLState(o.Region, runnerACLState{ClusterID: strconv.FormatUint(cid, 10), IP: ip, Modified: false})
					return lerr
				}
			}
			return writeRunnerACLState(o.Region, runnerACLState{ClusterID: strconv.FormatUint(cid, 10), IP: ip, Modified: false})
		}

		next, _ := acl.WithIP(ip)
		revision, err := client.PutControlPlaneACL(ctx, cid, next)
		if err != nil {
			lastErr = err
			fmt.Fprintf(os.Stderr, "::warning::runner-acl(open): PUT attempt %d failed (%v); re-reading and retrying.\n", attempt, err)
			aclSleep(aclRetryDelay)
			continue
		}
		// Verify-after-write: confirm a concurrent writer didn't clobber our add.
		if verify, gerr := client.GetControlPlaneACL(ctx, cid); gerr == nil && !verify.ContainsIP(ip) {
			lastErr = fmt.Errorf("ACL did not retain %s after PUT (racing writer)", ip)
			fmt.Fprintf(os.Stderr, "::warning::runner-acl(open): %s missing after PUT attempt %d (racing writer clobbered it); retrying.\n", ip, attempt)
			aclSleep(aclRetryDelay)
			continue
		}
		fmt.Printf("runner-acl(open): added %s to cluster %d control-plane ACL (revision %s).\n", ip, cid, revision)
		// The address list is the DESIRED state; it says nothing about whether the
		// control plane enforces it yet. Wait for the revision to be reflected —
		// the API's only enforcement signal — before claiming access works.
		if werr := waitACLEnforced(ctx, client, cid, revision); werr != nil {
			fmt.Fprintf(os.Stderr, "::warning::runner-acl(open): %v\n", werr)
		}
		// Lease it so the internal-CIDR firewall controller's next reconcile
		// preserves the IP instead of replacing it out from under a long-running
		// kubectl job.
		if o.ConfigMap {
			if lerr := registerRunnerACLIP(ip, reassertRunnerACL(ctx, client, cid, ip)); lerr != nil {
				// State FIRST: this invocation did add the IP, so the paired
				// revoke must still know to remove it even though we fail here.
				_ = writeRunnerACLState(o.Region, runnerACLState{ClusterID: strconv.FormatUint(cid, 10), IP: ip, Modified: true})
				return lerr
			}
		}
		return writeRunnerACLState(o.Region, runnerACLState{ClusterID: strconv.FormatUint(cid, 10), IP: ip, Modified: true})
	}
	return fmt.Errorf("failed to add %s to cluster %d control-plane ACL after %d attempts: %w", ip, cid, aclMaxAttempts, lastErr)
}

// reassertRunnerACL returns a closure that re-adds ip to cluster cid's
// control-plane ACL. Handed to registerRunnerACLIP so a failed lease write can
// undo an eviction that happened between the add above and the lease — see the
// deadlock note on registerRunnerACLIP. Re-reads the ACL each time so it unions
// with whatever the controller last wrote rather than restoring a stale set.
func reassertRunnerACL(ctx context.Context, client aclClient, cid uint64, ip string) reassertACL {
	return func() error {
		cur, err := client.GetControlPlaneACL(ctx, cid)
		if err != nil {
			return err
		}
		if cur.ContainsIP(ip) {
			return nil // not evicted; nothing to do
		}
		next, _ := cur.WithIP(ip)
		fmt.Fprintf(os.Stderr, "::warning::runner-acl: %s was evicted from cluster %d ACL (controller reconcile) — re-adding before the next lease attempt.\n", ip, cid)
		// Revision discarded: this is a mid-loop repair, and enforcement of the
		// re-add is covered by the caller's own API-readiness wait.
		_, perr := client.PutControlPlaneACL(ctx, cid, next)
		return perr
	}
}

func runnerACLRevoke(ctx context.Context, client aclClient, o ACLOpts) error {
	st, ok, err := readRunnerACLState(o.Region)
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
	if o.ConfigMap && st.IP != "" {
		deregisterRunnerACLIP(st.IP)
	}
	if !st.Modified || st.IP == "" || st.ClusterID == "" {
		fmt.Printf("runner-acl(revoke): nothing recorded as opened — no-op.\n")
		return removeRunnerACLState(o.Region)
	}
	cid, err := strconv.ParseUint(st.ClusterID, 10, 64)
	if err != nil {
		return fmt.Errorf("state file has a non-numeric cluster_id %q: %w", st.ClusterID, err)
	}

	// Read-modify-write with verify-after-write, mirroring open: each attempt
	// re-reads the CURRENT ACL (preserving a racer's concurrent additions),
	// removes our IP, PUTs, then confirms our IP is actually gone — a racer that
	// PUT a stale list could otherwise re-introduce it. Revoke stays TOLERANT:
	// it never returns a hard error (it runs under `if: always()`, so a non-nil
	// return would fail an otherwise-color.Green job); on exhausted retries it warns
	// and leaves the state file so a later revoke can retry.
	for attempt := 1; attempt <= aclMaxAttempts; attempt++ {
		acl, err := client.GetControlPlaneACL(ctx, cid)
		if err != nil {
			if attempt == aclMaxAttempts {
				fmt.Fprintf(os.Stderr, "::warning::runner-acl(revoke): GET ACL for cluster %d failed (%v); %s may persist — prune manually.\n", cid, err, st.IP)
				return nil
			}
			aclSleep(aclRetryDelay)
			continue
		}
		next, changed := acl.WithoutIP(st.IP)
		if !changed {
			fmt.Printf("runner-acl(revoke): %s absent from cluster %d ACL — no change.\n", st.IP, cid)
			return removeRunnerACLState(o.Region)
		}
		if _, err := client.PutControlPlaneACL(ctx, cid, next); err != nil {
			if attempt == aclMaxAttempts {
				fmt.Fprintf(os.Stderr, "::warning::runner-acl(revoke): PUT ACL for cluster %d failed (%v); %s may still be allowed — prune manually.\n", cid, err, st.IP)
				return nil
			}
			aclSleep(aclRetryDelay)
			continue
		}
		// Verify-after-write: confirm a concurrent writer didn't re-add our IP.
		if verify, gerr := client.GetControlPlaneACL(ctx, cid); gerr == nil && verify.ContainsIP(st.IP) {
			if attempt == aclMaxAttempts {
				fmt.Fprintf(os.Stderr, "::warning::runner-acl(revoke): %s still present after PUT for cluster %d (racing writer re-added it); may persist — prune manually.\n", st.IP, cid)
				return nil
			}
			fmt.Fprintf(os.Stderr, "::warning::runner-acl(revoke): %s reappeared after PUT attempt %d (racing writer); retrying.\n", st.IP, attempt)
			aclSleep(aclRetryDelay)
			continue
		}
		fmt.Printf("runner-acl(revoke): removed %s from cluster %d control-plane ACL.\n", st.IP, cid)
		return removeRunnerACLState(o.Region)
	}
	return nil
}

// listClustersWithRetry wraps the cluster-list lookup in the same bounded retry
// the ACL read-modify-write uses. A transient transport blip (connection reset,
// TLS timeout) on this single GET would otherwise fail cluster resolution with a
// misleading "no cluster matched" and block every cluster-touching job that
// opens the control-plane ACL. The caller's match logic stays single-shot, so a
// genuinely empty result (a definitive "not found") is not retried.
func listClustersWithRetry(ctx context.Context, lister clusterLister) ([]map[string]any, error) {
	var clusters []map[string]any
	var err error
	for attempt := 1; attempt <= aclMaxAttempts; attempt++ {
		if clusters, err = lister.ListClusters(ctx); err == nil {
			return clusters, nil
		}
		if attempt < aclMaxAttempts {
			fmt.Fprintf(os.Stderr, "::warning::list LKE clusters attempt %d/%d failed (%v); retrying.\n",
				attempt, aclMaxAttempts, err)
			aclSleep(aclRetryDelay)
		}
	}
	return nil, err
}

// resolveClusterID returns the target cluster's numeric ID from r.ClusterID, else
// r.ClusterLabel (+ r.LinodeRegion), else cluster_label/region read from
// <tfvarsDir>/<region>.tfvars — mirroring the action's resolve_cluster_id.
func resolveClusterID(ctx context.Context, lister clusterLister, r ClusterRef) (uint64, error) {
	if r.ClusterID != "" {
		id, err := strconv.ParseUint(r.ClusterID, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("--cluster-id %q is not numeric: %w", r.ClusterID, err)
		}
		return id, nil
	}
	label, lregion := r.ClusterLabel, r.LinodeRegion
	if (label == "" || lregion == "") && r.Region != "" {
		path := filepath.Join(r.TfvarsDir, r.Region+".tfvars")
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
		return 0, fmt.Errorf("cannot determine cluster label (no --cluster-id, no --cluster-label, no cluster_label in %s/%s.tfvars)", r.TfvarsDir, r.Region)
	}
	clusters, err := listClustersWithRetry(ctx, lister)
	if err != nil {
		return 0, fmt.Errorf("listing LKE clusters: %w", err)
	}
	ids := linode.MatchClusterIDs(clusters, label, lregion)
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return 0, fmt.Errorf("no LKE cluster matched label=%q linode-region=%q (env=%q); pass --cluster-id or --linode-region", label, lregion, r.Region)
	default:
		return 0, fmt.Errorf("%d clusters matched label=%q linode-region=%q (ambiguous); pass --cluster-id explicitly: %v", len(ids), label, lregion, ids)
	}
}

// ── egress IP detection ──────────────────────────────────────────────────────

// detectEgressIP returns this runner's public IPv4 and reports whether the probes
// AGREE about it.
//
// WHY IT ASKS ALL THREE INSTEAD OF STOPPING AT THE FIRST. A GitHub-hosted runner
// sits behind Azure SNAT with a POOL of egress addresses, and the address chosen
// can differ per destination. So the IP that api.ipify.org sees is not
// necessarily the IP the LKE apiserver sees — and if they differ we allowlist an
// address that never connects, producing precisely
//
//	dial tcp <apiserver>:6443: i/o timeout
//
// while the ACL, its revision, and the read-back all look correct. That is the
// exact signature seen on clusters 637276/637285/637289/637329/637367, where the
// Cloud Manager UI confirmed our IP present under our own revision-id.
//
// Disagreement between probes is therefore load-bearing evidence, not noise, and
// stopping at the first answer threw it away. This still RETURNS one IP (changing
// the ACL to a multi-address set touches the revoke contract), but it now logs
// every answer and says plainly when they diverge.
func detectEgressIP() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	seen := map[string][]string{} // ip -> probes reporting it
	var order []string
	for _, u := range []string{"https://api.ipify.org", "https://checkip.amazonaws.com", "https://ifconfig.me/ip"} {
		resp, err := client.Get(u)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runner-acl: egress probe %s failed: %v\n", u, err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		ip := strings.TrimSpace(string(b))
		p := net.ParseIP(ip)
		if p == nil || p.To4() == nil {
			fmt.Fprintf(os.Stderr, "runner-acl: egress probe %s returned non-IPv4 %q\n", u, ip)
			continue
		}
		fmt.Printf("runner-acl: egress probe %s -> %s\n", u, ip)
		if _, dup := seen[ip]; !dup {
			order = append(order, ip)
		}
		seen[ip] = append(seen[ip], u)
	}
	if len(order) == 0 {
		return "", fmt.Errorf("none of the egress-IP probes returned an IPv4 address")
	}
	if len(order) > 1 {
		fmt.Fprintf(os.Stderr, "::warning::runner-acl: egress probes DISAGREE (%v) — this runner has multiple "+
			"public egress addresses, so the one allowlisted may not be the one the apiserver sees. That produces "+
			"an `i/o timeout` to :6443 while the ACL looks correct. Using %s; if kubectl then times out, allowlist "+
			"all of %v (or use a runner with a stable egress).\n", order, order[0], order)
	}
	return order[0], nil
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

// aclEnforceWait / aclEnforcePoll bound the enforcement wait. The docs say
// enabling an ACL "may take up to 20 minutes for the ACL rules to take effect",
// but this runs inside a step budget, so it waits a useful slice and then hands
// off to the caller's own API-readiness wait rather than blocking the whole job.
var (
	aclEnforceWait = 4 * time.Minute
	aclEnforcePoll = 10 * time.Second
)

// waitACLEnforced polls until the control plane reflects revision back.
//
// DO NOT TREAT A PASS HERE AS PROOF OF REACHABILITY. The name says ENFORCED
// because that is the API's documented contract — the submitted revision-id is
// said to appear on GET "when (and only after) the ACL stanza is verified as
// enforced". MEASURED BEHAVIOUR CONTRADICTS THAT, and the original rationale for
// this function ("the only real signal available") was wrong:
//
//   - On a settled cluster the revision is reflected essentially IMMEDIATELY,
//     while kubectl is still being refused. Timed on a live cluster: ACL remove →
//     blocked within 25s; ACL add → access restored in ~35s. The revision LEADS
//     reality; it does not trail it.
//   - Run 13's log shows the failure mode changing from `i/o timeout` (SYN
//     dropped) to `EOF` (connected, then closed) around 60s after the PUT — so
//     there are at least two enforcement stages, and the revision handshake
//     tracks neither.
//
// So this is a DIAGNOSTIC, not a gate: it makes the log distinguish "the API
// acknowledged our revision" from "it never did", which was previously invisible
// and cost five runs of confusion. What actually establishes access is the
// caller's own wait-for-API step, and that is deliberately where the real waiting
// happens.
//
// A timeout is therefore a WARNING, not an error — doubly so given the documented
// window is up to 20 minutes, longer than any single step should hold. Do not
// "fix" that by making it fatal: the signal is not reliable enough to gate on.
//
// Historical note: three commits in this area (57130891, 908f8e0d, 95ca3c39)
// attribute the e2e hangs to ACL enforcement. They fixed real defects, but the
// attribution was wrong — run 8's hang predates the first of them, and the
// address was in fact present in the ACL throughout.
func waitACLEnforced(ctx context.Context, client aclClient, cid uint64, revision string) error {
	if revision == "" {
		return nil // nothing to track (older API or a no-change path)
	}
	deadline := time.Now().Add(aclEnforceWait)
	for attempt := 1; ; attempt++ {
		cur, err := client.GetControlPlaneACL(ctx, cid)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "runner-acl(open): enforcement check %d failed: %v\n", attempt, err)
		case cur.ACLEnforced(revision):
			fmt.Printf("runner-acl(open): ACL revision %s is ENFORCED — the control plane will accept this runner.\n", revision)
			return nil
		default:
			fmt.Printf("runner-acl(open): ACL revision %s not yet enforced (control plane reports %q) — waiting...\n",
				revision, cur.RevisionID)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ACL revision %s was not reported enforced within %s. The address IS in the ACL, but "+
				"the control plane had not applied it yet — enforcement is documented as taking up to 20 minutes. "+
				"kubectl will keep failing (timeout/EOF) until it lands; the API-readiness wait continues from here",
				revision, aclEnforceWait)
		}
		aclSleep(aclEnforcePoll)
	}
}
