package linode

// reap.go ports the orphan-reaper suite (instance-scripts/lib-linode.sh + the
// cleanup-orphan-*.sh + reap-all-orphaned-resources.sh) into the native client.
// It is split into PURE orphan-identity heuristics (exported + unit-tested here —
// the fiddly logic that drifted between bash copies) and thin API primitives
// (list/delete) the `llz reap` orchestrator drives in dependency order.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ── pure orphan-identity heuristics ──────────────────────────────────────────

var nbTagRe = regexp.MustCompile(`^lke-?([0-9]+)$`)

// LKEIDFromLabel returns the cluster id embedded in an `lke<digits>` VPC label
// (e.g. "lke613260" -> "613260"); "" if the label is not exactly lke+digits.
func LKEIDFromLabel(label string) string {
	if !strings.HasPrefix(label, "lke") {
		return ""
	}
	rest := label[len("lke"):]
	if rest == "" || !isAllDigits(rest) {
		return ""
	}
	return rest
}

// LKEIDFromTags returns the cluster id from a NodeBalancer's CCM tag
// (`lke<id>` or `lke-<id>`); "" if no such tag is present.
func LKEIDFromTags(tags []string) string {
	for _, t := range tags {
		if m := nbTagRe.FindStringSubmatch(t); m != nil {
			return m[1]
		}
	}
	return ""
}

// LKEClusterIDFromNB returns the id of the LKE cluster that owns a NodeBalancer,
// read from its `lke_cluster` object; "" if absent. This is the RELIABLE cluster
// linkage: LKE-E CCM NodeBalancers are tagged only `kubernetes` (no `lke<id>`
// tag), so tag-based attribution misses them — but lke_cluster.id always points
// to the owning cluster, and persists as a dangling ref after that cluster is
// deleted, making it a definitive orphan signal.
func LKEClusterIDFromNB(m map[string]any) string {
	lc, ok := m["lke_cluster"].(map[string]any)
	if !ok {
		return ""
	}
	return mIDString(lc)
}

// NBDecision is the result of classifying a NodeBalancer's orphan status.
type NBDecision int

const (
	NBKeep          NBDecision = iota // not an orphan
	NBOrphan                          // its CCM cluster-id tag points to a gone cluster
	NBCheckBackends                   // CCM-identified, no cluster tag — orphan iff 0 backends
)

// ClassifyNodeBalancer decides a NodeBalancer's orphan status. lkeClusterID is
// its lke_cluster.id (LKEClusterIDFromNB), the reliable owner link; tags/label
// are the CCM fallbacks. Resolution order:
//   - lke_cluster.id present  -> keep iff that cluster is live, else orphan.
//   - CCM `lke<id>` tag        -> keep iff live, else orphan (older CCMs).
//   - ccm-* label / kubernetes -> NBCheckBackends (the caller treats 0 backends
//     as orphan) — last resort for an NB with neither owner field.
//
// The lke_cluster.id branch is what catches LKE-E CCM NodeBalancers, which carry
// only the `kubernetes` tag and so previously fell to the backend check (and were
// never deleted by the tag-scoped destroy sweep).
func ClassifyNodeBalancer(lkeClusterID string, tags []string, label string, live map[string]bool) NBDecision {
	if lkeClusterID != "" {
		if live[lkeClusterID] {
			return NBKeep
		}
		return NBOrphan
	}
	if cid := LKEIDFromTags(tags); cid != "" {
		if live[cid] {
			return NBKeep
		}
		return NBOrphan
	}
	if strings.HasPrefix(label, "ccm-") || containsStr(tags, "kubernetes") {
		return NBCheckBackends
	}
	return NBKeep
}

// VPCIsOrphan reports whether an `lke<id>` VPC's cluster is gone. (The module's
// "<label>-vpc" BYO VPCs carry no id and are matched by label by the caller.)
func VPCIsOrphan(label string, live map[string]bool) bool {
	cid := LKEIDFromLabel(label)
	return cid != "" && !live[cid]
}

// VolumeIsCandidate mirrors cleanup-orphan-volumes.sh's safe filter: an
// unattached `pvc-*` Volume, optionally constrained by region, an id allowlist,
// and a required tag. An empty regionFilter / idAllow / tagMustInclude means
// "no constraint".
func VolumeIsCandidate(linodeIDNull bool, label, region string, tags []string,
	regionFilter string, idAllow map[string]bool, id, tagMustInclude string) bool {
	if !linodeIDNull || !strings.HasPrefix(label, "pvc-") {
		return false
	}
	if regionFilter != "" && region != regionFilter {
		return false
	}
	if len(idAllow) > 0 && !idAllow[id] {
		return false
	}
	if tagMustInclude != "" && !containsStr(tags, tagMustInclude) {
		return false
	}
	return true
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// ── API primitives ───────────────────────────────────────────────────────────

// LiveClusterIDs returns the id (as a string) of every live LKE cluster — the
// set that is NEVER treated as an orphan.
func (c *Client) LiveClusterIDs(ctx context.Context) (map[string]bool, error) {
	cl, err := c.listAllPages(ctx, "/v4beta/lke/clusters")
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(cl))
	for _, m := range cl {
		if id := mIDString(m); id != "" {
			live[id] = true
		}
	}
	return live, nil
}

// ClustersWithLabel returns the ids of LIVE LKE clusters whose label matches.
func (c *Client) ClustersWithLabel(ctx context.Context, label string) ([]uint64, error) {
	cl, err := c.listAllPages(ctx, "/v4beta/lke/clusters")
	if err != nil {
		return nil, err
	}
	var ids []uint64
	for _, m := range cl {
		if mString(m, "label") == label {
			ids = append(ids, mUint(m, "id"))
		}
	}
	return ids, nil
}

// NodeBalancerBackendCount sums up+down backend nodes across all of a
// NodeBalancer's configs. 0 => the CCM has no cluster to register backends from.
func (c *Client) NodeBalancerBackendCount(ctx context.Context, id uint64) (int, error) {
	cfgs, err := c.listAllPages(ctx, fmt.Sprintf("/v4/nodebalancers/%d/configs", id))
	if err != nil {
		return 0, err
	}
	total := 0
	for _, cfg := range cfgs {
		if ns, ok := cfg["nodes_status"].(map[string]any); ok {
			total += int(mUint(ns, "up")) + int(mUint(ns, "down"))
		}
	}
	return total, nil
}

// List helpers return the raw resource maps for a collection.
func (c *Client) ListNodeBalancers(ctx context.Context) ([]map[string]any, error) {
	return c.listAllPages(ctx, "/v4/nodebalancers")
}
func (c *Client) ListVPCs(ctx context.Context) ([]map[string]any, error) {
	return c.listAllPages(ctx, "/v4/vpcs")
}
func (c *Client) ListVPCSubnets(ctx context.Context, vpcID uint64) ([]map[string]any, error) {
	return c.listAllPages(ctx, fmt.Sprintf("/v4/vpcs/%d/subnets", vpcID))
}
func (c *Client) ListVolumes(ctx context.Context) ([]map[string]any, error) {
	return c.listAllPages(ctx, "/v4/volumes")
}
func (c *Client) ListFirewalls(ctx context.Context) ([]map[string]any, error) {
	return c.listAllPages(ctx, "/v4/networking/firewalls")
}

// DeleteResourcePath DELETEs an absolute API path (e.g. "/v4/nodebalancers/123").
// A 2xx or 404 (already gone) is success — matching lib-linode.sh's linode_delete.
func (c *Client) DeleteResourcePath(ctx context.Context, path string) error {
	resp, err := c.del(ctx, c.base+path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == 404 {
		return nil
	}
	return fmt.Errorf("DELETE %s returned %d: %s", path, resp.StatusCode, readBody(resp))
}

// ── map accessors (json.Number-aware) ────────────────────────────────────────

func mString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func mUint(m map[string]any, k string) uint64 {
	switch v := m[k].(type) {
	case json.Number:
		i, _ := v.Int64()
		return uint64(i)
	case float64:
		return uint64(v)
	}
	return 0
}

func mIDString(m map[string]any) string {
	if id := mUint(m, "id"); id != 0 {
		return strconv.FormatUint(id, 10)
	}
	return ""
}

// MapTags extracts a resource's string tags (exported for the orchestrator).
func MapTags(m map[string]any) []string {
	raw, _ := m["tags"].([]any)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if s, ok := t.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// MapString / MapUint / MapIDString expose the accessors to the orchestrator.
func MapString(m map[string]any, k string) string { return mString(m, k) }
func MapUint(m map[string]any, k string) uint64   { return mUint(m, k) }
func MapIDString(m map[string]any) string         { return mIDString(m) }

// VolumeLinodeIDNull reports whether a Volume map has linode_id == null
// (unattached) — JSON null decodes to a nil interface value.
func VolumeLinodeIDNull(m map[string]any) bool {
	v, present := m["linode_id"]
	return !present || v == nil
}
