package openbao

// backup_restore.go — `llz openbao backups` and `llz openbao restore`.
//
// The operator half of OpenBao backup. The CronJob that WRITES snapshots is the
// openbao-backup extension; this lists them and puts one back. Both sides go
// through internal/shared/baosnap, and the e2e restore drill calls the same
// Fetch / Inspect / Apply this file does — so the gate exercises this command's
// path rather than a copy of it.
//
// NOTHING HERE READS OPENBAO TO FIND THE BACKUPS. The day this command matters is
// the day OpenBao is gone, so the bucket is located from the instance spec (label
// prefix + deployment + object-storage cluster) and read with a temporary
// read-only key minted from LINODE_API_TOKEN, which lives outside OpenBao.
//
// THE GUARDS, because restore REPLACES the whole store:
//
//   - dry run by default: it fetches, verifies, and reports which snapshot would
//     overwrite which live cluster, then stops. --yes executes.
//   - --yes is not enough: --confirm restore:<deployment> must name the
//     deployment, so a command copied from a runbook for one cluster cannot be
//     replayed against another.
//   - a snapshot must be chosen explicitly: --snapshot <key>, or --latest. There
//     is no default — "the newest" is a choice, and after an incident it is often
//     the wrong one (it may already contain the damage). --latest is refused when
//     the bucket holds more than one OpenBao cluster's snapshots (after a rebuild
//     the newest is the rebuilt cluster's empty store) unless --from-cluster says
//     whose history is meant — see baosnap.Select.
//   - the snapshot must verify before it is sent.
//   - the kube-context must point at THIS deployment's LKE cluster. --region picks
//     the bucket; the kube-context picks the target; nothing else ties the two.
//     The LKE cluster id read off the context's nodes must be the one the Linode
//     account labels with the spec's cluster.clusterLabel for --region — checked
//     on the dry run and again before applying, and NOT waived by --force (which
//     answers a different question).
//   - a snapshot whose cluster id is not the live cluster's needs --force. In an
//     HA pair the two clusters are INDEPENDENT stores with their own keyrings; a
//     mismatch is the peer (wrong kube-context) as often as a rebuild.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

// restoreLKEClusters lists the ids of the LKE clusters in the account carrying
// label. Seamed for tests; built from the restore binding like every other handle.
var restoreLKEClusters = func(ctx context.Context, token, label string) ([]uint64, error) {
	return capability.CloudFor(restoreBinding()).Client(token, 30*time.Second).ClustersWithLabel(ctx, label)
}

// restoreBinding is the declaration every handle here is built from.
func restoreBinding() extension.Binding { return Extension().MustBinding("restore") }

// Seams for tests.
var (
	restoreKeys = func(token string) baosnap.KeyAPI {
		return capability.CloudFor(restoreBinding()).Client(token, 30*time.Second)
	}
	restoreCluster = func() capability.Cluster { return capability.For(restoreBinding()).Cluster }
	restorePF      = openbao.PortForwardPod
	restoreStore   = baosnap.TempStore
	restoreSettle  = 3 * time.Minute
	restoreNow     = time.Now
)

// location is where a deployment's snapshots live, and which LKE cluster they
// belong on, all from the spec.
type location struct {
	objCluster, bucket, endpoint string
	// clusterLabel is the deployment's LKE cluster label (cluster.clusterLabel) —
	// what the kube-context check holds the restore target to.
	clusterLabel string
}

// backupLocation resolves where a deployment's snapshots live, from the spec.
func backupLocation(region string) (location, error) {
	if region == "" {
		return location{}, fmt.Errorf("--region is required")
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return location{}, fmt.Errorf("load the instance spec (run from the instance repo root): %w", err)
	}
	e, ok := lz.Env(region)
	if !ok {
		return location{}, fmt.Errorf("no deployment %q in the spec", region)
	}
	oc := e.Cluster.ObjectStorage.Cluster
	if oc == "" {
		return location{}, fmt.Errorf("deployment %q declares no cluster.objectStorage.cluster", region)
	}
	return location{
		objCluster: oc, bucket: clusterspec.ObjOpenBaoBackupBucket(lz.ObjLabelPrefix(), region),
		endpoint: clusterspec.ObjEndpointHost(oc), clusterLabel: e.Cluster.ClusterLabel,
	}, nil
}

func openBackupStore(ctx context.Context, region string) (baosnap.Store, func(), error) {
	loc, err := backupLocation(region)
	if err != nil {
		return baosnap.Store{}, func() {}, err
	}
	objCluster, bucket, endpoint := loc.objCluster, loc.bucket, loc.endpoint
	tok, err := linode.TokenFromEnv()
	if err != nil {
		return baosnap.Store{}, func() {}, fmt.Errorf("%w — the backups are read with a temporary key minted from it, because the backup key itself lives in OpenBao", err)
	}
	fmt.Fprintf(os.Stderr, "→ minting a temporary read-only key on %s (deleted on exit)…\n", bucket)
	return restoreStore(ctx, restoreKeys(tok), "llz-restore-"+region, objCluster, bucket, endpoint, "read_only")
}

// RunBackups lists a deployment's snapshots, newest first.
func RunBackups(region string) error {
	ctx := context.Background()
	st, revoke, err := openBackupStore(ctx, region)
	if err != nil {
		return err
	}
	defer revoke()
	objs, err := st.List(ctx, baosnap.Prefix)
	if err != nil {
		return err
	}
	snaps, foreign := baosnap.Catalog(objs)
	if len(snaps) == 0 {
		return fmt.Errorf("%s holds no snapshots under %s — the openbaoBackup CronJob has never uploaded one for %s. "+
			"Check `kubectl -n %s get cronjob,jobs` and its logs", st.Bucket, baosnap.Prefix, region, baosnap.Namespace)
	}
	now := restoreNow()
	fmt.Printf("OpenBao snapshots for %s in %s (newest first):\n", region, st.Bucket)
	for _, s := range snaps {
		fmt.Println("  " + baosnap.Describe(s, now))
	}
	if ids := baosnap.ClusterIDs(snaps); len(ids) > 1 {
		fmt.Printf("\nNOTE: these come from %d different OpenBao clusters (%s). That is what a rebuild leaves behind: the\n"+
			"newest are usually the rebuilt cluster's backups of its own (empty) store. `restore --latest` needs\n"+
			"--from-cluster <id> here; see docs/runbooks/openbao-backup-restore.md.\n", len(ids), strings.Join(ids, ", "))
	}
	if len(foreign) > 0 {
		fmt.Printf("(%d other object(s) under %s are not snapshot names and are ignored: %s)\n",
			len(foreign), baosnap.Prefix, strings.Join(foreign, ", "))
	}
	return nil
}

// RestoreOpts are the restore flags.
type RestoreOpts struct {
	Region      string
	Snapshot    string
	Latest      bool
	FromCluster string
	Force       bool
	Confirm     string
	Yes         bool
}

// RunRestore is `llz openbao restore`.
func RunRestore(o RestoreOpts) error {
	if (o.Snapshot == "") == !o.Latest {
		return fmt.Errorf("choose the snapshot explicitly: --snapshot <key> (see `llz openbao backups --region %s`) or --latest", o.Region)
	}
	ctx := context.Background()
	s, err := prepareRestore(ctx, o)
	if err != nil {
		return err
	}
	defer s.Close()

	if !o.Yes {
		force := ""
		if !s.plan.SameCluster {
			force = " --force"
		}
		fmt.Printf("\nDRY RUN — nothing changed. To restore, re-run with --yes --confirm restore:%s%s\n", o.Region, force)
		return nil
	}
	if o.Confirm != "restore:"+o.Region {
		return fmt.Errorf("--confirm must be exactly restore:%s — it names the deployment whose OpenBao is about to be replaced", o.Region)
	}
	root := os.Getenv("OPENBAO_ROOT_TOKEN")
	if root == "" {
		return fmt.Errorf("OPENBAO_ROOT_TOKEN is not set — a restore needs root on the TARGET cluster. " +
			"Regenerate one with `llz openbao regen-root` (recovery-key quorum), export it, and re-run")
	}
	if err := s.apply(ctx, root); err != nil {
		return err
	}
	fmt.Print(postRestoreChecklist(o.Region, haPeer(o.Region), s.snap, s.now))
	return nil
}

// restoreSession is a restore between its two halves: everything has been
// fetched, verified and inspected, and nothing has been changed. It holds the
// temporary bucket key and the tunnel to the active pod until Close.
//
// TWO HALVES BECAUSE THERE ARE TWO CALLERS. `llz openbao restore` runs them back
// to back. The restore-openbao workflow (`llz ci openbao-restore`) puts the one
// thing it must not waste between them — a root token minted from the recovery
// quorum — so that every refusal prepareRestore can reach (no such snapshot, an
// archive that does not verify, a kube-context on another deployment, a foreign
// snapshot without --force) happens before a quorum is spent.
type restoreSession struct {
	o      RestoreOpts
	snap   baosnap.Snapshot
	data   []byte
	m      baosnap.Manifest
	cl     capability.Cluster
	t      baosnap.Target
	plan   baosnap.Plan
	now    time.Time
	closer []func()
}

// Close releases the tunnel and deletes the temporary bucket key.
func (s *restoreSession) Close() {
	for i := len(s.closer) - 1; i >= 0; i-- {
		s.closer[i]()
	}
	s.closer = nil
}

// prepareRestore is everything up to the irreversible call: fetch and verify the
// snapshot, prove the kube-context is --region's LKE cluster, find the active
// pod, and read the live cluster. It prints the plan. On error nothing is left
// open.
func prepareRestore(ctx context.Context, o RestoreOpts) (s *restoreSession, err error) {
	s = &restoreSession{o: o}
	defer func() {
		if err != nil {
			s.Close()
			s = nil
		}
	}()
	st, revoke, err := openBackupStore(ctx, o.Region)
	if err != nil {
		return s, err
	}
	s.closer = append(s.closer, revoke)

	s.snap, s.data, s.m, err = baosnap.Fetch(ctx, st, o.Snapshot, o.FromCluster)
	if err != nil {
		return s, err
	}
	s.now = restoreNow()
	fmt.Printf("snapshot   %s\n           %d bytes, raft index %d term %d, sealed hashes %v — verified\n",
		baosnap.Describe(s.snap, s.now), len(s.data), s.m.Index, s.m.Term, s.m.Sealed)

	s.cl = restoreCluster()
	ctxName, _ := s.cl.Run("config", "current-context")
	lke, err := verifyTargetCluster(ctx, s.cl, o.Region)
	if err != nil {
		return s, err
	}
	s.t, err = baosnap.OpenActive(s.cl.Run, restorePF)
	if err != nil {
		return s, err
	}
	s.closer = append(s.closer, s.t.Close)
	s.plan, err = baosnap.Inspect(ctx, s.t, s.snap, s.m)
	if err != nil {
		return s, err
	}
	fmt.Printf("target     kube-context %q = %s, OpenBao cluster %s (%s) initialized=%v sealed=%v\n",
		strings.TrimSpace(string(ctxName)), lke, s.plan.Live.ClusterID, s.plan.Live.ClusterName, s.plan.Live.Initialized, s.plan.Live.Sealed)
	if s.plan.SameCluster {
		fmt.Println("match      the snapshot was taken by this cluster")
	} else {
		fmt.Printf("MISMATCH   the snapshot was taken by cluster %s, not this one — restoring needs --force\n", s.snap.ClusterID)
	}
	if peer := haPeer(o.Region); peer != "" {
		fmt.Printf("HA pair    %s is paired with %s. They are independent stores: this restores %s ONLY.\n", o.Region, peer, o.Region)
	}
	return s, nil
}

// apply is the irreversible half: re-prove the kube-context, then restore with
// root and wait for OpenBao to serve again.
func (s *restoreSession) apply(ctx context.Context, root string) error {
	// Again, immediately before the one irreversible call: a dry run read minutes
	// ago says nothing about where the context points now.
	if _, err := verifyTargetCluster(ctx, s.cl, s.o.Region); err != nil {
		return err
	}
	fmt.Printf("restoring %s into %s…\n", s.snap.Key, s.o.Region)
	if err := baosnap.Apply(ctx, s.t, root, s.plan, s.data, s.o.Force, restoreSettle); err != nil {
		return err
	}
	fmt.Printf("✓ restored %s — OpenBao is unsealed and serving the snapshot's state (raft index %d).\n", s.snap.Key, s.m.Index)
	return nil
}

// verifyTargetCluster proves the kube-context points at --region's LKE cluster:
// the cluster id its nodes carry must be one the Linode account labels with the
// spec's cluster.clusterLabel for that deployment. Returns a description for the
// dry-run output.
//
// WHY THIS IS NOT THE CLUSTER-ID CHECK. The OpenBao cluster-id comparison asks
// "did this store take this snapshot?" and --force exists to answer "no, it was
// rebuilt" — at which point it no longer guards anything, and a rebuild is exactly
// when an operator juggles several contexts. This asks "is the target the
// deployment the bucket belongs to?", which has the same answer before and after a
// rebuild, so nothing waives it.
func verifyTargetCluster(ctx context.Context, cl capability.Cluster, region string) (string, error) {
	loc, err := backupLocation(region)
	if err != nil {
		return "", err
	}
	if loc.clusterLabel == "" {
		return "", fmt.Errorf("deployment %q declares no cluster.clusterLabel — cannot tell whether the kube-context points at it, so refusing", region)
	}
	nodes, nerr := cl.Run("get", "nodes", "-o", "jsonpath={.items[*].metadata.name}")
	server, _ := cl.Run("config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}")
	liveID, err := LKEClusterOfContext(strings.Fields(string(nodes)), strings.TrimSpace(string(server)))
	if err != nil {
		if nerr != nil {
			err = fmt.Errorf("%w (listing nodes: %v)", err, nerr)
		}
		return "", err
	}
	tok, err := linode.TokenFromEnv()
	if err != nil {
		return "", err
	}
	ids, err := restoreLKEClusters(ctx, tok, loc.clusterLabel)
	if err != nil {
		return "", fmt.Errorf("look up LKE cluster %q in the Linode account: %w", loc.clusterLabel, err)
	}
	if err := TargetIsDeployment(region, loc.clusterLabel, liveID, ids); err != nil {
		return "", err
	}
	return fmt.Sprintf("LKE cluster %s (%s)", liveID, loc.clusterLabel), nil
}

// lkeServerRe matches an LKE-Enterprise API server host, which leads with the
// cluster id: https://lke637329.api.us-ord.enterprise.linodelke.net:6443.
var lkeServerRe = regexp.MustCompile(`^https?://lke([0-9]+)\.`)

// LKEClusterOfContext reads the LKE cluster id a kube-context points at: from its
// nodes' names (`lke<cluster>-<pool>-<suffix>`, linode.LKEClusterIDFromNodeName —
// the same parser discovery uses) and, when there are none to read, from an
// LKE-Enterprise API server host. Every node must agree; a context whose nodes
// name two clusters, or none, is not something to restore into. Pure.
func LKEClusterOfContext(nodeNames []string, server string) (string, error) {
	seen := map[string]bool{}
	var unparsed []string
	for _, n := range nodeNames {
		if id := linode.LKEClusterIDFromNodeName(n); id != "" {
			seen[id] = true
		} else {
			unparsed = append(unparsed, n)
		}
	}
	switch {
	case len(seen) > 1:
		ids := make([]string, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return "", fmt.Errorf("the kube-context's nodes belong to more than one LKE cluster (%s) — refusing to pick one", strings.Join(ids, ", "))
	case len(seen) == 1 && len(unparsed) == 0:
		for id := range seen {
			return id, nil
		}
	case len(seen) == 1:
		return "", fmt.Errorf("the kube-context's nodes are not all LKE nodes of one cluster (unrecognised: %s) — refusing", strings.Join(unparsed, ", "))
	}
	if m := lkeServerRe.FindStringSubmatch(server); m != nil && len(nodeNames) == 0 {
		return m[1], nil
	}
	return "", fmt.Errorf("cannot tell which LKE cluster the kube-context points at (nodes %v, server %q) — refusing to restore into an unidentified cluster", nodeNames, server)
}

// TargetIsDeployment is the verdict: liveID must be one of the account's clusters
// labelled clusterLabel. Pure.
func TargetIsDeployment(region, clusterLabel, liveID string, idsWithLabel []uint64) error {
	if len(idsWithLabel) == 0 {
		return fmt.Errorf("no LKE cluster labelled %q (deployment %s's cluster.clusterLabel) exists in this Linode account, "+
			"but the kube-context points at LKE cluster %s — refusing: that is not %s", clusterLabel, region, liveID, region)
	}
	want := make([]string, 0, len(idsWithLabel))
	for _, id := range idsWithLabel {
		if fmt.Sprint(id) == liveID {
			return nil
		}
		want = append(want, fmt.Sprint(id))
	}
	return fmt.Errorf("the kube-context points at LKE cluster %s, but deployment %s is LKE cluster %s (%s) — "+
		"--region chose %s's backups and the context would restore them somewhere else. Switch your kube-context "+
		"to %s's cluster and re-run; --force does not waive this", liveID, region, strings.Join(want, ", "), clusterLabel, region, region)
}

// postRestoreChecklist is what the operator must do next. Printed, not
// automated: every item depends on what changed since the snapshot, which only
// the operator knows.
func postRestoreChecklist(region, peer string, snap baosnap.Snapshot, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nNEXT — the store is now as it was %s ago:\n", now.Sub(snap.Taken).Round(time.Minute))
	b.WriteString("  1. The root token you just used is gone (it was minted after the snapshot). Tokens, leases and\n" +
		"     recovery-key configuration are the snapshot's. Revoke nothing by hand; regenerate root only if needed.\n")
	b.WriteString("  2. In-cluster rotated credentials (secret/obj/*, secret/linode/*) may now name keys the rotators have\n" +
		"     since REVOKED. Force a re-mint: see docs/runbooks/openbao-backup-restore.md, \"After a restore\".\n")
	b.WriteString("  3. Re-apply any `llz openbao set` written after the snapshot.\n")
	if peer != "" {
		fmt.Fprintf(&b, "  4. %s is an HA pair with %s: run the drift check in docs/secrets.md against both and re-apply\n"+
			"     the newer values with `llz openbao set` (which dual-writes).\n", region, peer)
	}
	return b.String()
}

// haPeer returns the deployment's HA peer, or "" for a standalone one (or when
// the topology cannot be read — this only shapes advice).
func haPeer(region string) string {
	deps, err := envtopology.ReadTopology("terraform-iac-bootstrap")
	if err != nil {
		return ""
	}
	p, ok, err := envtopology.PeerOf(deps, region)
	if err != nil || !ok {
		return ""
	}
	return p
}
