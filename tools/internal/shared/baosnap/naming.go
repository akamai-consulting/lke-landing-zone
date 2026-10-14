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

// Prune returns the snapshot keys retention deletes: everything past the keep
// newest. Three refusals, each a scar default:
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
	var out []string
	// The just-written snapshot occupies one of the keep slots up front, wherever
	// its timestamp sorts — so keep means "this run's snapshot plus keep-1 others".
	kept := 1
	for _, s := range snaps {
		if s.Key == justWritten {
			continue
		}
		if kept < keep {
			kept++
			continue
		}
		out = append(out, s.Key)
	}
	return out
}

// Select picks the snapshot a restore uses: the named key, or the newest when key
// is "" (the caller has already required an explicit --latest for that). It never
// falls back from a named key to another one — a typo must not restore something
// other than what was asked for.
func Select(snaps []Snapshot, key string) (Snapshot, error) {
	if len(snaps) == 0 {
		return Snapshot{}, fmt.Errorf("the bucket holds no snapshots under %s", Prefix)
	}
	if key == "" {
		return snaps[0], nil
	}
	for _, s := range snaps {
		if s.Key == key {
			return s, nil
		}
	}
	return Snapshot{}, fmt.Errorf("no snapshot %q in the bucket (newest is %s)", key, snaps[0].Key)
}
