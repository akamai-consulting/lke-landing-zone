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
//     the wrong one (it may already contain the damage).
//   - the snapshot must verify before it is sent.
//   - a snapshot whose cluster id is not the live cluster's needs --force. In an
//     HA pair the two clusters are INDEPENDENT stores with their own keyrings; a
//     mismatch is the peer (wrong kube-context) as often as a rebuild.

import (
	"context"
	"fmt"
	"os"
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

// backupLocation resolves where a deployment's snapshots live, from the spec.
func backupLocation(region string) (objCluster, bucket, endpoint string, err error) {
	if region == "" {
		return "", "", "", fmt.Errorf("--region is required")
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return "", "", "", fmt.Errorf("load the instance spec (run from the instance repo root): %w", err)
	}
	e, ok := lz.Env(region)
	if !ok {
		return "", "", "", fmt.Errorf("no deployment %q in the spec", region)
	}
	objCluster = e.Cluster.ObjectStorage.Cluster
	if objCluster == "" {
		return "", "", "", fmt.Errorf("deployment %q declares no cluster.objectStorage.cluster", region)
	}
	return objCluster, clusterspec.ObjOpenBaoBackupBucket(lz.ObjLabelPrefix(), region), clusterspec.ObjEndpointHost(objCluster), nil
}

func openBackupStore(ctx context.Context, region string) (baosnap.Store, func(), error) {
	objCluster, bucket, endpoint, err := backupLocation(region)
	if err != nil {
		return baosnap.Store{}, func() {}, err
	}
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
	if len(foreign) > 0 {
		fmt.Printf("(%d other object(s) under %s are not snapshot names and are ignored: %s)\n",
			len(foreign), baosnap.Prefix, strings.Join(foreign, ", "))
	}
	return nil
}

// RestoreOpts are the restore flags.
type RestoreOpts struct {
	Region   string
	Snapshot string
	Latest   bool
	Force    bool
	Confirm  string
	Yes      bool
}

// RunRestore is `llz openbao restore`.
func RunRestore(o RestoreOpts) error {
	if (o.Snapshot == "") == !o.Latest {
		return fmt.Errorf("choose the snapshot explicitly: --snapshot <key> (see `llz openbao backups --region %s`) or --latest", o.Region)
	}
	ctx := context.Background()
	st, revoke, err := openBackupStore(ctx, o.Region)
	if err != nil {
		return err
	}
	defer revoke()

	snap, data, m, err := baosnap.Fetch(ctx, st, o.Snapshot)
	if err != nil {
		return err
	}
	now := restoreNow()
	fmt.Printf("snapshot   %s\n           %d bytes, raft index %d term %d, sealed hashes %v — verified\n",
		baosnap.Describe(snap, now), len(data), m.Index, m.Term, m.Sealed)

	cl := restoreCluster()
	ctxName, _ := cl.Run("config", "current-context")
	t, err := baosnap.OpenActive(cl.Run, restorePF)
	if err != nil {
		return err
	}
	defer t.Close()
	plan, err := baosnap.Inspect(ctx, t, snap, m)
	if err != nil {
		return err
	}
	fmt.Printf("target     kube-context %q, OpenBao cluster %s (%s) initialized=%v sealed=%v\n",
		strings.TrimSpace(string(ctxName)), plan.Live.ClusterID, plan.Live.ClusterName, plan.Live.Initialized, plan.Live.Sealed)
	if plan.SameCluster {
		fmt.Println("match      the snapshot was taken by this cluster")
	} else {
		fmt.Printf("MISMATCH   the snapshot was taken by cluster %s, not this one — restoring needs --force\n", snap.ClusterID)
	}
	if peer := haPeer(o.Region); peer != "" {
		fmt.Printf("HA pair    %s is paired with %s. They are independent stores: this restores %s ONLY.\n", o.Region, peer, o.Region)
	}

	if !o.Yes {
		force := ""
		if !plan.SameCluster {
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
	fmt.Printf("restoring %s into %s…\n", snap.Key, o.Region)
	if err := baosnap.Apply(ctx, t, root, plan, data, o.Force, restoreSettle); err != nil {
		return err
	}
	fmt.Printf("✓ restored %s — OpenBao is unsealed and serving the snapshot's state (raft index %d).\n", snap.Key, m.Index)
	fmt.Print(postRestoreChecklist(o.Region, haPeer(o.Region), snap, now))
	return nil
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
