package main

// objcluster_resolve.go — turn `--obj-cluster` from an identifier the operator has
// to invent into one `llz env add` derives, or at least checks.
//
// Why this exists. The value is not cosmetic and it is not guessable. A Linode
// region exposes SEVERAL object-storage endpoints of different generations (E0/E1
// legacy, E2/E3 standard), and a bucket is reachable ONLY at the endpoint it was
// created against — so `us-ord-1` and `us-ord-10` are two disjoint namespaces, not
// two spellings of one. Pick the wrong one and the apply SUCCEEDS: Loki and Harbor
// provision buckets nobody can reach, and it surfaces much later as NoSuchBucket.
//
// Until now the only gate was validate.OBJClusterID, which checks the SHAPE and
// says so ("It does NOT constrain the ordinal"), pointing the operator at
// `linode-cli object-storage clusters-list` to work it out themselves. The account
// already knows the answer and llz already has the API call, so ask.
//
// Best-effort by construction. `llz env add` has never needed a Linode token or
// network, and must not start: with no token, an unreachable API, or an ambiguous
// answer, this degrades to exactly the previous behaviour. It can make the flow
// better; it can never make it fail where it used to work.

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// objClusterLister is the slice of the Linode client this needs — a seam so the
// tests never touch the network.
type objClusterLister interface {
	ListObjectStorageClusters(ctx context.Context) ([]map[string]any, error)
}

// objClusterClient returns a live client, or nil when no token is configured.
// Package var so tests substitute a fake.
var objClusterClient = func() objClusterLister {
	tok := firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("LINODE_API_TOKEN"))
	if tok == "" {
		return nil
	}
	return linode.NewClient(tok, 20*time.Second)
}

// objClustersInRegion returns the account's object-storage cluster ids in region,
// sorted. ok is false when the answer is unknown (no token, API error) — which is
// NOT the same as "none exist", and callers must not treat it as one.
func objClustersInRegion(region string) (ids []string, ok bool) {
	c := objClusterClient()
	if c == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	all, err := c.ListObjectStorageClusters(ctx)
	if err != nil {
		return nil, false
	}
	for _, m := range all {
		if asStr(m["region"]) != region {
			continue
		}
		if id := asStr(m["id"]); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, true
}

// resolveOBJCluster decides the obj-cluster for `llz env add`. Returns the value to
// use, plus a human note to print (empty when there is nothing worth saying).
//
// Supplied value  → checked against the region's real list; a value that is not in
//
//	it is rejected WITH the alternatives, because that is the
//	mistake nothing downstream catches.
//
// Empty value     → derived when the region offers exactly ONE cluster. With
//
//	several it refuses and lists them rather than guessing: the
//	choice between generations is the whole hazard, and picking
//	for the operator would just move the silent failure.
func resolveOBJCluster(objCluster, region string) (string, string, error) {
	ids, ok := objClustersInRegion(region)

	if objCluster != "" {
		if err := instancelayout.ValidateOBJCluster(objCluster); err != nil {
			return "", "", fmt.Errorf("--obj-cluster: %w", err)
		}
		if !ok || len(ids) == 0 {
			// Unknown, not wrong — keep the previous behaviour and say why the
			// stronger check did not run.
			return objCluster, "", nil
		}
		if !slices.Contains(ids, objCluster) {
			//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline carrying the fix instructions
			return "", "", fmt.Errorf(
				"--obj-cluster %q is not an object-storage cluster in region %s.\n"+
					"  This account offers: %s\n"+
					"  A bucket is reachable ONLY at the endpoint it was created against, so a\n"+
					"  wrong id here provisions Loki/Harbor buckets their consumers cannot read —\n"+
					"  the apply succeeds and it surfaces later as NoSuchBucket.",
				objCluster, region, strings.Join(ids, ", "))
		}
		return objCluster, fmt.Sprintf("obj-cluster %s confirmed against the account's clusters in %s.", objCluster, region), nil
	}

	// Nothing supplied.
	if !ok {
		//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline carrying the fix instructions
		return "", "", fmt.Errorf("--obj-cluster is required (the spec's cluster.objectStorage.cluster).\n" +
			"  Set LINODE_TOKEN and llz will derive it from your account; otherwise list them with\n" +
			"  `linode-cli object-storage clusters-list` and pass --obj-cluster explicitly.")
	}
	switch len(ids) {
	case 0:
		//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline carrying the fix instructions
		return "", "", fmt.Errorf("--obj-cluster is required: this account has no object-storage cluster in region %s.\n"+
			"  Check the region, or enable Object Storage there first.", region)
	case 1:
		return ids[0], fmt.Sprintf("obj-cluster %s derived — the only object-storage cluster in %s.", ids[0], region), nil
	default:
		//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline carrying the fix instructions
		return "", "", fmt.Errorf("--obj-cluster is required: region %s offers more than one object-storage cluster (%s).\n"+
			"  These are DIFFERENT endpoint generations and a bucket is reachable only at the one it\n"+
			"  was created against, so llz will not choose for you. Pick one and pass --obj-cluster.",
			region, strings.Join(ids, ", "))
	}
}

// asStr renders a JSON scalar as a string without pulling in the cli helper's
// wider behaviour (only strings matter here).
func asStr(v any) string {
	s, _ := v.(string)
	return s
}
