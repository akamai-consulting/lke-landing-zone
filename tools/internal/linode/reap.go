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
	"time"
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

// VolDecision is the result of classifying a Volume's cluster-ownership.
type VolDecision int

const (
	VolKeep     VolDecision = iota // carries an `lke<id>` tag for a LIVE cluster — never an orphan
	VolOrphan                      // carries an `lke<id>` tag for a GONE cluster — a definitive orphan
	VolUntagged                    // no `lke<id>` cluster tag — no ownership signal; caller falls back to the scope filter
)

// ClassifyVolume decides a Volume's orphan status from its cluster-ownership tag —
// the `lke<id>` tag the block-storage-retain StorageClass stamps on every PVC's
// Volume at provision time (CSI volumeTags; same convention the CCM uses for
// NodeBalancers, so LKEIDFromTags parses both). This is the cluster-liveness gate
// that makes a broad (region-wide) Volume sweep safe: a *detached* `pvc-*` Volume
// whose owning cluster is still live is NOT an orphan — it is a Retain-policy Volume
// of a running cluster (a pod rescheduling, a node replaced, a chart mid-upgrade)
// and must be kept. Without a cluster tag we cannot attribute the Volume, so the
// caller keeps it by default (see the fail-safe VolUntagged handling in cmd/llz) —
// a Volume born on a class without the tag, or by other tooling, is never assumed
// to be an orphan.
//
// TRADE-OFF (VolKeep is deliberately broad): this gate cannot tell a Volume that
// is detached because of live-cluster churn (pod reschedule, node replace) from
// one that is detached because its PVC was permanently deleted while the cluster
// lives on under `reclaimPolicy: Retain`. Both are VolKeep, so a genuinely
// abandoned Retain Volume of a LIVE cluster is never auto-reaped by the
// account-level sweep and will accumulate cost until reclaimed out-of-band (the
// `--volume-ids` allowlist, or the Linode UI). That is the intended bias — never
// risk deleting an in-use Volume. Closing it safely needs an in-cluster reconciler
// that enumerates live PVs (a Volume tagged to the cluster yet absent from its PV
// set is a provably abandoned Retain Volume); account-level reap cannot make the call.
func ClassifyVolume(tags []string, live map[string]bool) VolDecision {
	cid := LKEIDFromTags(tags)
	if cid == "" {
		return VolUntagged
	}
	if live[cid] {
		return VolKeep
	}
	return VolOrphan
}

// UntaggedVolumeReapGrace is how long an UNTAGGED detached `pvc-*` Volume is spared
// from reaping even under --reap-untagged. CSI stamps the `lke<id>` tag in the
// CreateVolume call, so a Volume should never be observed untagged; this grace only
// covers a brief window where the Linode API might list a just-created Volume before
// its tags settle — reaping there would be the exact data loss the liveness gate
// exists to prevent. The default (keep all untagged) already covers this; the grace
// is a second belt for the --reap-untagged path.
const UntaggedVolumeReapGrace = 30 * time.Minute

// VolumeYoungerThan reports whether a Volume's `created` timestamp is within d of
// now — the age guard applied ONLY to VolUntagged Volumes (VolKeep is already kept,
// VolOrphan is a definitive gone-cluster orphan regardless of age). Linode returns
// `created` as RFC3339, historically without a zone (implicitly UTC); both parse.
// A missing/unparseable timestamp yields false — i.e. fall back to the existing
// scope-filter behaviour (reap-eligible) rather than protecting a Volume forever
// on a signal we can't read.
func VolumeYoungerThan(created string, now time.Time, d time.Duration) bool {
	if created == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, created)
	if err != nil {
		// Zone-less form parses as UTC, which is what the Linode API means.
		if t, err = time.Parse("2006-01-02T15:04:05", created); err != nil {
			return false
		}
	}
	return now.Sub(t) < d
}

// VolumeIsCandidate mirrors cleanup-orphan-volumes.sh's safe filter: an
// unattached `pvc-*` Volume, optionally constrained by region, an id allowlist,
// and a required tag. An empty regionFilter / idAllow / tagMustInclude means
// "no constraint". It is the SCOPE filter only — the cluster-liveness gate that
// distinguishes a live cluster's detached Volume from a true orphan is
// ClassifyVolume, which the caller applies on top of this.
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

// UpdateVolumeLabel PUTs a new UI label onto a Linode Volume. A 404 — the volume
// was deleted out-of-band while a PV still references it — is treated as success
// (nothing to relabel), matching relabel.sh's skip-404 behavior.
func (c *Client) UpdateVolumeLabel(ctx context.Context, id uint64, label string) error {
	url := fmt.Sprintf("%s/v4/volumes/%d", c.base, id)
	resp, err := c.put(ctx, url, map[string]string{"label": label})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("PUT /v4/volumes/%d returned %d: %s", id, resp.StatusCode, readBody(resp))
	}
	return nil
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
