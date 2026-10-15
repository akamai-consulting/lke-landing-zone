package baosnap

// naming.go — what a snapshot object is called, and which ones retention keeps.
//
// The key carries the two facts a restore needs before it downloads anything:
// WHEN the snapshot was taken, and WHICH OpenBao cluster it came from. The second
// is what makes the HA-pair guard possible. Each deployment of a pair runs its own
// independent Raft cluster with its own barrier keyring and its own static seal
// key; a snapshot from one is not a backup of the other. `llz openbao restore`
// compares the key's cluster id with the live cluster's and refuses a mismatch
// without --force — which is the one prompt standing between an operator with the
// wrong kube-context and a cross-restore of the peer's data.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Prefix is the folder snapshots live under. Everything retention may delete is
// inside it AND parses as a snapshot name; an object an operator drops elsewhere
// in the bucket (a hand-taken snapshot, a note) is never touched.
const Prefix = "openbao-raft/"

const (
	suffix       = ".snap"
	stampLayout  = "20060102T150405Z"
	unknownClust = "unknown"
)

var (
	// clusterIDSafe keeps the key S3-plain. OpenBao cluster ids are UUIDs, so this
	// only ever bites a value that is not one — which is then visibly mangled
	// rather than silently producing a key the lister cannot parse back.
	clusterIDSafe = regexp.MustCompile(`[^a-z0-9-]+`)
	keyRe         = regexp.MustCompile(`^` + regexp.QuoteMeta(Prefix) + `([0-9]{8}T[0-9]{6}Z)_([a-z0-9-]+)` + regexp.QuoteMeta(suffix) + `$`)
)

// ObjectKey names the snapshot taken at t from the cluster with this id.
//
// The timestamp leads so a plain lexical LIST is already chronological — useful to
// an operator reading the bucket with any S3 tool, not just to this package.
func ObjectKey(t time.Time, clusterID string) string {
	id := clusterIDSafe.ReplaceAllString(strings.ToLower(strings.TrimSpace(clusterID)), "-")
	id = strings.Trim(id, "-")
	if id == "" {
		id = unknownClust
	}
	return Prefix + t.UTC().Format(stampLayout) + "_" + id + suffix
}

// Snapshot is one parsed snapshot object.
type Snapshot struct {
	Key       string
	Taken     time.Time
	ClusterID string
	Size      int64
}

// ParseObjectKey is ObjectKey's inverse. ok=false for anything not produced by
// ObjectKey — those objects are listed as foreign, never pruned, never chosen.
func ParseObjectKey(key string) (Snapshot, bool) {
	m := keyRe.FindStringSubmatch(key)
	if m == nil {
		return Snapshot{}, false
	}
	t, err := time.Parse(stampLayout, m[1])
	if err != nil {
		return Snapshot{}, false
	}
	return Snapshot{Key: key, Taken: t, ClusterID: m[2]}, true
}

// Object is one listed bucket object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Catalog splits a listing into snapshots (newest first) and foreign keys.
func Catalog(objs []Object) (snaps []Snapshot, foreign []string) {
	for _, o := range objs {
		s, ok := ParseObjectKey(o.Key)
		if !ok {
			foreign = append(foreign, o.Key)
			continue
		}
		s.Size = o.Size
		snaps = append(snaps, s)
	}
	sort.SliceStable(snaps, func(i, j int) bool {
		if !snaps[i].Taken.Equal(snaps[j].Taken) {
			return snaps[i].Taken.After(snaps[j].Taken)
		}
		return snaps[i].Key > snaps[j].Key
	})
	sort.Strings(foreign)
	return snaps, foreign
}

// Prune returns the snapshot keys retention deletes. Retention is PER CLUSTER ID:
// every OpenBao cluster that ever wrote to this bucket keeps its own newest keep
// snapshots, and nothing one cluster writes can push another's out.
//
// WHY PER CLUSTER, NOT "THE NEWEST keep IN THE BUCKET". After a deployment's
// OpenBao is lost and rebuilt, the rebuilt cluster is a NEW Raft cluster (new id)
// whose store is empty until someone restores it. Its CronJob keeps ticking into
// the same bucket. Under a bucket-wide keep-newest-28 those empty-store snapshots
// counted toward the 28, so about seven days after the rebuild every snapshot of
// the data that was actually lost had been deleted — by the backup job, on
// schedule, while every alert stayed green. Keyed by cluster id, the pre-loss
// cluster's newest keep are never this run's to delete: only a run BY that
// cluster can age them out, and that cluster is gone.
//
// The cost is that a bucket keeps up to keep snapshots of every cluster id it has
// ever seen. A rebuild is rare, and an operator who has finished recovering can
// delete a dead cluster's snapshots by hand; the backup job never will.
//
// Three more refusals, each a scar default:
//
//   - keep is floored at 1. "Keep zero" is a typo, not a retention policy, and on
//     this bucket it would mean every run deletes the backup it just made.
//   - the key just written is never pruned, whatever its timestamp says. A pod
//     with a skewed clock writes a key that sorts as old; retention must not
//     delete the only snapshot this run produced because of it.
//   - nothing is pruned when the just-written key is absent from the listing.
//     That listing did not see our own write, so it is not evidence about the
//     bucket — deleting on it would trade a known-good backup for a guess.
func Prune(snaps []Snapshot, keep int, justWritten string) []string {
	if keep < 1 {
		keep = 1
	}
	seen := false
	for _, s := range snaps {
		if s.Key == justWritten {
			seen = true
			break
		}
	}
	if !seen {
		return nil
	}
	ours, _ := ParseObjectKey(justWritten)
	var out []string
	// kept counts, per cluster id, the snapshots retention has decided to keep.
	// The just-written snapshot occupies one of its cluster's slots up front,
	// wherever its timestamp sorts — so keep means "this run's snapshot plus
	// keep-1 others" for this run's cluster, and plain "the newest keep" for every
	// other cluster id in the bucket.
	kept := map[string]int{ours.ClusterID: 1}
	for _, s := range snaps {
		if s.Key == justWritten {
			continue
		}
		if kept[s.ClusterID] < keep {
			kept[s.ClusterID]++
			continue
		}
		out = append(out, s.Key)
	}
	return out
}

// ClusterIDs returns the distinct cluster ids in snaps, in the order their newest
// snapshot appears (snaps is newest first, as Catalog returns it).
func ClusterIDs(snaps []Snapshot) []string {
	seen := map[string]bool{}
	var ids []string
	for _, s := range snaps {
		if !seen[s.ClusterID] {
			seen[s.ClusterID] = true
			ids = append(ids, s.ClusterID)
		}
	}
	return ids
}

// Select picks the snapshot a restore uses.
//
//   - key names one exactly. It never falls back from a named key to another one
//     — a typo must not restore something other than what was asked for — and a
//     fromCluster that disagrees with the named key's cluster id is an error, not
//     a filter that quietly finds nothing.
//   - key "" means the newest (the caller has already required an explicit
//     --latest for that) — of fromCluster's snapshots when it is set.
//
// "THE NEWEST" IS REFUSED WHEN IT IS AMBIGUOUS. If the bucket holds snapshots from
// more than one OpenBao cluster and fromCluster is empty, there is no single
// "latest": after a rebuild, the newest object is the REBUILT cluster's backup of
// its own empty store, and its id even matches the live cluster's, so a dry run
// would report "match" on exactly the snapshot that restores nothing. The operator
// has to say which cluster's history they mean.
func Select(snaps []Snapshot, key, fromCluster string) (Snapshot, error) {
	if len(snaps) == 0 {
		return Snapshot{}, fmt.Errorf("the bucket holds no snapshots under %s", Prefix)
	}
	fromCluster = strings.ToLower(strings.TrimSpace(fromCluster))
	if key != "" {
		for _, s := range snaps {
			if s.Key == key {
				if fromCluster != "" && s.ClusterID != fromCluster {
					return Snapshot{}, fmt.Errorf("snapshot %s was taken by cluster %s, not --from-cluster %s", key, s.ClusterID, fromCluster)
				}
				return s, nil
			}
		}
		return Snapshot{}, fmt.Errorf("no snapshot %q in the bucket (newest is %s)", key, snaps[0].Key)
	}
	ids := ClusterIDs(snaps)
	if fromCluster == "" {
		if len(ids) > 1 {
			return Snapshot{}, fmt.Errorf("the bucket holds snapshots from %d OpenBao clusters, so \"latest\" is ambiguous — %s. "+
				"After a rebuild the newest is usually the REBUILT cluster's backup of its own empty store. "+
				"Name the cluster whose history you want with --from-cluster <id>, or pick a key with --snapshot",
				len(ids), describeClusters(snaps, ids))
		}
		return snaps[0], nil
	}
	for _, s := range snaps {
		if s.ClusterID == fromCluster {
			return s, nil
		}
	}
	return Snapshot{}, fmt.Errorf("no snapshot from cluster %s in the bucket — it holds %s", fromCluster, describeClusters(snaps, ids))
}

// describeClusters summarises each cluster id's newest snapshot and count, for an
// error that has to tell the operator what IS there.
func describeClusters(snaps []Snapshot, ids []string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		n, newest := 0, time.Time{}
		for _, s := range snaps {
			if s.ClusterID == id {
				n++
				if s.Taken.After(newest) {
					newest = s.Taken
				}
			}
		}
		parts = append(parts, fmt.Sprintf("cluster %s: %d snapshot(s), newest %s", id, n, newest.UTC().Format(time.RFC3339)))
	}
	return strings.Join(parts, "; ")
}
