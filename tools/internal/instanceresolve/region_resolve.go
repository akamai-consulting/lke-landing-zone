package instanceresolve

// region_resolve.go — check `llz env add --region` against the account before a
// spec is authored against it.
//
// Why a shape check cannot do this. `us-sea` is a region and `us-sea-1` is an
// object-storage cluster, but `de-fra-2` and `jp-tyo-3` are ALSO regions — the
// two identifier spaces overlap, so no regex distinguishes them. The quickstart
// puts them adjacent on one line (`--region us-sea --obj-cluster us-sea-1`),
// which makes swapping them the natural mistake, and a swapped or typo'd region
// is caught by nothing local: the spec only checks that cluster.region is
// non-empty, and the first thing that notices is the cluster apply.
//
// Best-effort, exactly like objcluster_resolve.go: with no token, an unreachable
// API, or an empty answer this degrades to the previous behaviour (no check). It
// can improve the flow; it must never fail a run that used to work.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// regionLister is the slice of the Linode client this needs — a seam so tests
// never touch the network.
type regionLister interface {
	ListRegions(ctx context.Context) ([]map[string]any, error)
}

// RegionClient returns a live client, or nil when no token is configured.
// Package var so tests substitute a fake.
var RegionClient = func() regionLister {
	tok := firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("LINODE_API_TOKEN"))
	if tok == "" {
		return nil
	}
	return linode.NewClient(tok, 20*time.Second)
}

// accountRegions returns the account's region ids, sorted. ok is false when the
// answer is unknown (no token, API error, empty list) — which is NOT the same as
// "the region does not exist", and callers must not treat it as one.
func accountRegions() (ids []string, ok bool) {
	c := RegionClient()
	if c == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	all, err := c.ListRegions(ctx)
	if err != nil || len(all) == 0 {
		return nil, false
	}
	for _, m := range all {
		if id := asStr(m["id"]); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, len(ids) > 0
}

// CheckRegion rejects a --region the account does not have, listing the regions
// that look like what was asked for. nil whenever the answer is unknown.
func CheckRegion(region string) error {
	ids, ok := accountRegions()
	if !ok {
		return nil
	}
	for _, id := range ids {
		if id == region {
			return nil
		}
	}
	msg := fmt.Sprintf("--region %q is not a Linode region.", region)
	if near := nearbyRegions(region, ids); len(near) > 0 {
		msg += fmt.Sprintf("\n  Did you mean: %s", strings.Join(near, ", "))
	}
	// The swap the quickstart's own one-liner invites: the OBJ cluster id landed
	// in --region. Name it, because "not a region" alone reads like a typo.
	// Phrased as a resemblance, not a fact: all this knows is that the value is
	// "<a real region>-<ordinal>", which is the SHAPE of an OBJ cluster id — it
	// has not asked the object-storage API whether this particular one exists.
	if err := instancelayout.ValidateOBJCluster(region); err == nil && IsOBJClusterID(region, ids) {
		base := region[:strings.LastIndex(region, "-")]
		msg += fmt.Sprintf("\n  %q is shaped like an object-storage CLUSTER id in region %s — did you mean `--region %s --obj-cluster %s`?",
			region, base, base, region)
	}
	msg += "\n  List them with `linode-cli regions list`."
	return fmt.Errorf("%s", msg)
}

// IsOBJClusterID reports whether region parses as "<a real region>-<ordinal>" —
// the signature of an object-storage cluster id supplied where a region belongs.
// Checking the prefix against the account's real regions is what keeps a genuine
// numeric-suffixed region (de-fra-2) from being mislabelled: it is in ids, so
// CheckRegion never gets here for it.
func IsOBJClusterID(region string, ids []string) bool {
	i := strings.LastIndex(region, "-")
	if i < 0 {
		return false
	}
	for _, id := range ids {
		if id == region[:i] {
			return true
		}
	}
	return false
}

// nearbyRegions returns the account regions that share a prefix with what was
// asked for (at most three) — enough to fix a typo without printing the whole
// global region list.
func nearbyRegions(region string, ids []string) []string {
	prefix := region
	if i := strings.Index(region, "-"); i > 0 {
		prefix = region[:i]
	}
	var out []string
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			out = append(out, id)
			if len(out) == 3 {
				break
			}
		}
	}
	return out
}
