package teardown

// openbao_backup_guard.go — `llz ci openbao-backup-destroy-guard`: the destroy of a
// deployment's object storage may not take its OpenBao backups with it by default.
//
// WHY THIS IS NOT ONE MORE BUCKET IN THE DRAIN. The Loki and Harbor buckets are
// the cluster's working storage; destroying object-storage is consent to lose
// them. The openbao-backup bucket is the opposite: it exists to OUTLIVE the
// cluster, and "destroy object-storage" is exactly what a team runs while
// rebuilding after a loss — the one moment the backups are the only copy. So the
// destroy job runs this first, and it:
//
//   - says nothing is there (no bucket in the account, or a bucket holding no
//     object version at all) and lets the destroy proceed — clearing any bare
//     delete markers, without which a versioned bucket cannot be deleted;
//   - REFUSES while any version remains, unless the caller passed the explicit
//     opt-in (--allow-delete, from the workflow's drain_data_buckets). Noncurrent
//     versions count: a pruned snapshot is still a recoverable one for the
//     bucket's noncurrent-version window;
//   - with the opt-in, deletes every version and delete marker, then LISTS AGAIN
//     and fails unless the bucket really is empty — a delete that answered 204 is
//     the producer's word, the re-list is the consumer's.
//
// It fails closed: a listing it cannot read, a key that never becomes usable, or
// an account listing that errors all stop the destroy rather than wave it through.
//
// It runs BEFORE the drain step and before anything is deleted, so a refusal
// leaves every bucket exactly as it was.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baosnap"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// backupGuardAPI is the slice of the Linode client the guard uses: find the
// bucket, and mint/revoke the temporary key it reads (and, with consent, empties)
// it with.
type backupGuardAPI interface {
	baosnap.KeyAPI
	ListObjectStorageBuckets(ctx context.Context) ([]map[string]any, error)
}

// Seams for tests. The client comes from the destroying transition's binding —
// minting a key is a mutation even when the guard then only reads.
var (
	backupGuardClient = func(token string) backupGuardAPI {
		return capability.CloudFor(cloudBinding(true)).Client(token, 30*time.Second)
	}
	backupGuardStore = baosnap.TempStore
)

// BackupGuardVerdict is the decision over a version listing. Pure.
//
// data is the number of real object versions (current or noncurrent); markers the
// number of delete markers. It returns whether to delete everything, or the
// refusal.
func BackupGuardVerdict(bucket string, data, markers int, allowDelete bool) (deleteAll bool, err error) {
	switch {
	case data == 0:
		// Nothing recoverable is here. Bare delete markers still block the bucket
		// delete, so clear them.
		return markers > 0, nil
	case !allowDelete:
		return false, fmt.Errorf("refusing to destroy object-storage: %s holds %d OpenBao snapshot version(s) — the backups of this "+
			"deployment's OpenBao, which this destroy would delete with the bucket. Nothing has been drained or deleted. "+
			"Copy them out first if you may need them (`llz openbao backups --region <deployment>` lists them), then re-run "+
			"with drain_data_buckets=true to delete them", bucket, data)
	default:
		return true, nil
	}
}

// RunOpenBaoBackupGuard is the verb. allowDelete is the consent (which backups may
// go); yes is the global --yes every destructive teardown verb also requires (that
// this run may delete at all) — two answers to two questions, as extension.go
// argues for Confirm.
func RunOpenBaoBackupGuard(region string, allowDelete, yes bool) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	if allowDelete && !yes {
		return fmt.Errorf("--allow-delete deletes every OpenBao backup of %q — pass --yes as well", region)
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return fmt.Errorf("load the instance spec (run from the instance repo root): %w", err)
	}
	e, ok := lz.Env(region)
	if !ok {
		return fmt.Errorf("no deployment %q in the spec", region)
	}
	objCluster := e.Cluster.ObjectStorage.Cluster
	if objCluster == "" {
		return fmt.Errorf("deployment %q declares no cluster.objectStorage.cluster", region)
	}
	bucket := clusterspec.ObjOpenBaoBackupBucket(lz.ObjLabelPrefix(), region)
	token := os.Getenv("LINODE_API_TOKEN")
	if token == "" {
		return fmt.Errorf("LINODE_API_TOKEN must be set — the guard reads the backup bucket with a temporary key minted from it")
	}
	ctx := context.Background()
	api := backupGuardClient(token)

	all, err := api.ListObjectStorageBuckets(ctx)
	if err != nil {
		return fmt.Errorf("list the account's buckets (could not tell whether %s exists, so refusing): %w", bucket, err)
	}
	if !bucketExists(all, bucket, objCluster) {
		fmt.Printf("no %s in the account — no OpenBao backups to protect; the destroy may proceed.\n", bucket)
		return nil
	}

	perms := "read_only"
	if allowDelete {
		perms = "read_write"
	}
	st, revoke, err := backupGuardStore(ctx, api, "llz-destroy-guard-"+region, objCluster, bucket, clusterspec.ObjEndpointHost(objCluster), perms)
	if err != nil {
		return fmt.Errorf("could not read %s, so refusing to destroy it: %w", bucket, err)
	}
	defer revoke()

	vs, err := st.ListVersions(ctx)
	if err != nil {
		return fmt.Errorf("could not list %s, so refusing to destroy it: %w", bucket, err)
	}
	data, markers := countVersions(vs)
	deleteAll, err := BackupGuardVerdict(bucket, data, markers, allowDelete)
	if err != nil {
		return err
	}
	if !deleteAll {
		fmt.Printf("%s holds no object versions — nothing to protect; the destroy may proceed.\n", bucket)
		return nil
	}
	if data > 0 {
		fmt.Printf("drain_data_buckets=true: deleting %d OpenBao snapshot version(s) and %d delete marker(s) from %s.\n", data, markers, bucket)
	} else {
		fmt.Printf("clearing %d bare delete marker(s) from %s so the bucket can be deleted.\n", markers, bucket)
	}
	// With no consent, the key is read_only and only markers are to go — which a
	// read_only key cannot delete. Mint a write key for that narrow case.
	if !allowDelete {
		revoke()
		st, revoke, err = backupGuardStore(ctx, api, "llz-destroy-guard-"+region, objCluster, bucket, clusterspec.ObjEndpointHost(objCluster), "read_write")
		if err != nil {
			return err
		}
		defer revoke()
	}
	var failed []string
	for _, v := range vs {
		if !allowDelete && !v.DeleteMarker {
			// Defence in depth: without consent only tombstones may go. The verdict
			// already guarantees there is no data here (data == 0).
			continue
		}
		if err := st.DeleteVersion(ctx, v.Key, v.VersionID); err != nil {
			failed = append(failed, err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not delete %d version(s) from %s: %s", len(failed), bucket, strings.Join(failed, "; "))
	}
	left, err := st.ListVersions(ctx)
	if err != nil {
		return fmt.Errorf("re-list %s after deleting: %w", bucket, err)
	}
	if len(left) > 0 {
		return fmt.Errorf("%s still holds %d version(s) after deleting them all — the destroy would fail on it", bucket, len(left))
	}
	fmt.Printf("✓ %s is empty of every version; the destroy may delete it.\n", bucket)
	return nil
}

func countVersions(vs []baosnap.Version) (data, markers int) {
	for _, v := range vs {
		if v.DeleteMarker {
			markers++
		} else {
			data++
		}
	}
	return data, markers
}

// bucketExists reports whether the account listing holds label in objCluster's
// region. Bucket labels are unique per region, not per account.
func bucketExists(all []map[string]any, label, objCluster string) bool {
	region := objRegion(objCluster)
	for _, b := range all {
		if s, _ := b["label"].(string); s != label {
			continue
		}
		r, _ := b["region"].(string)
		c, _ := b["cluster"].(string)
		if r == "" && c == "" || r == region || objRegion(c) == region {
			return true
		}
	}
	return false
}

// objRegion strips the endpoint ordinal: us-ord-10 → us-ord (the same rule the
// object-storage module places buckets by).
func objRegion(cluster string) string {
	if i := strings.LastIndex(cluster, "-"); i > 0 {
		tail := cluster[i+1:]
		if tail != "" && strings.Trim(tail, "0123456789") == "" {
			return cluster[:i]
		}
	}
	return cluster
}
