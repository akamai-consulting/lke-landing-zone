package linode

// acl.go is the typed read-modify-write of an LKE cluster's control-plane ACL —
// the Go port of the lke-runner-acl composite action's curl+jq logic. The pure
// add/remove-IP helpers (the part the bash drifted on) are unit-tested; the
// `llz ci runner-acl` orchestrator wires them to the Linode API and the
// per-runner state file.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// ControlPlaneACL is an LKE cluster's control-plane ACL: whether access is
// restricted (Enabled) and the allowed IPv4 / IPv6 CIDR sets.
//
// The Linode endpoint is /v4beta/lke/clusters/{id}/control_plane_acl and wraps
// the fields in an {"acl": {...}} envelope — the documented endpoint the
// runner-acl read-modify-write was ported from. Get/PutControlPlaneACL are the
// single implementation of it; an earlier duplicate hit /control_plane/acl with
// an unwrapped body, which 404s on LKE-E.
type ControlPlaneACL struct {
	Enabled bool
	IPv4    []string
	IPv6    []string
	// RevisionID is the id of the LAST ACL revision the control plane has
	// verified as ENFORCED — not the desired state. See PutControlPlaneACL.
	RevisionID string
}

// GetControlPlaneACL reads the cluster's control-plane ACL. An absent `enabled`
// is reported as true (the ACL is enforced), matching the action's `// true`.
func (c *Client) GetControlPlaneACL(ctx context.Context, clusterID uint64) (ControlPlaneACL, error) {
	resp, err := c.get(ctx, "v4beta", fmt.Sprintf("lke/clusters/%d/control_plane_acl", clusterID))
	if err != nil {
		return ControlPlaneACL{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ControlPlaneACL{}, fmt.Errorf("GET control-plane ACL for cluster %d returned %d: %s",
			clusterID, resp.StatusCode, readBody(resp))
	}
	var body struct {
		ACL struct {
			Enabled    *bool  `json:"enabled"`
			RevisionID string `json:"revision-id"`
			Addresses  struct {
				IPv4 []string `json:"ipv4"`
				IPv6 []string `json:"ipv6"`
			} `json:"addresses"`
		} `json:"acl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ControlPlaneACL{}, fmt.Errorf("parsing control-plane ACL for cluster %d: %w", clusterID, err)
	}
	enabled := true
	if body.ACL.Enabled != nil {
		enabled = *body.ACL.Enabled
	}
	acl := ControlPlaneACL{
		Enabled:    enabled,
		IPv4:       body.ACL.Addresses.IPv4,
		IPv6:       body.ACL.Addresses.IPv6,
		RevisionID: body.ACL.RevisionID,
	}
	// An EMPTY revision-id on GET is itself the answer to "has anything been
	// enforced yet". Worth surfacing: if the field never populates, the
	// enforcement handshake is unavailable on this cluster and the caller's wait
	// would otherwise look like a hang with no explanation.
	if acl.RevisionID == "" {
		fmt.Fprintf(os.Stderr, "linode: GET control_plane_acl cluster=%d returned NO revision-id "+
			"(enabled=%v, %d ipv4 entr(ies)) — the control plane has not reported an enforced revision\n",
			clusterID, acl.Enabled, len(acl.IPv4))
	}
	return acl, nil
}

// PutControlPlaneACL replaces the cluster's control-plane ACL and returns the
// revision id it submitted. Address lists are forced non-nil so they marshal as
// [] rather than null.
//
// THE PUT IS ASYNCHRONOUS AND A READ-BACK OF `addresses` PROVES NOTHING.
// A GET immediately after this returns the DESIRED address list — it echoes what
// was accepted, not what the control plane enforces. The API contract is explicit:
// "The optional field revision-id provided will be reflected on GET response when
// (and only after) the ACL stanza is verified as enforced", and the docs state
// enabling an ACL "may take up to 20 minutes for the ACL rules to take effect".
//
// Verifying with ContainsIP alone is what made cluster-access racy: it reported
// "added <ip> to control-plane ACL" and proceeded, and every subsequent kubectl
// then timed out or EOF'd until enforcement caught up. It passed whenever
// enforcement happened to be fast (run 7: 48s) and failed whenever it was not
// (runs 8-12). Callers must wait for the returned id via waitACLEnforced.
func (c *Client) PutControlPlaneACL(ctx context.Context, clusterID uint64, acl ControlPlaneACL) (string, error) {
	revision := newACLRevisionID()
	body := map[string]any{
		"acl": map[string]any{
			"enabled":     acl.Enabled,
			"revision-id": revision,
			"addresses":   map[string]any{"ipv4": NonNil(acl.IPv4), "ipv6": NonNil(acl.IPv6)},
		},
	}
	url := fmt.Sprintf("%s/v4beta/lke/clusters/%d/control_plane_acl", c.base, clusterID)
	resp, err := c.put(ctx, url, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw := readBody(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("PUT control-plane ACL for cluster %d returned %d: %s",
			clusterID, resp.StatusCode, raw)
	}
	// LOG THE STATUS AND BODY. Checking only the 2xx BUCKET hid the most
	// important fact about this call: 200 means applied, 202 means QUEUED, and the
	// difference is exactly the asynchrony that broke cluster-access. The success
	// path previously printed "added <ip> to control-plane ACL" with no evidence
	// of what the API actually said, which is why five runs argued about whether
	// the grant had taken effect. The body is an ACL — addresses already logged
	// elsewhere — so there is nothing sensitive to redact.
	fmt.Fprintf(os.Stderr, "linode: PUT control_plane_acl cluster=%d revision=%s -> HTTP %d %s\n",
		clusterID, revision, resp.StatusCode, truncateForLog(raw, 400))
	return revision, nil
}

// truncateForLog keeps a response body loggable without flooding a run log.
func truncateForLog(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(truncated)"
}

// newACLRevisionID mints a revision id unique to this PUT. Seamed for tests.
var newACLRevisionID = func() string { return fmt.Sprintf("llz-%d", time.Now().UnixNano()) }

// ACLEnforced reports whether the control plane has verified revision as enforced.
func (a ControlPlaneACL) ACLEnforced(revision string) bool {
	return revision != "" && a.RevisionID == revision
}

// ContainsIP reports whether ip is in the ACL's IPv4 set, in bare or /32 form.
func (a ControlPlaneACL) ContainsIP(ip string) bool {
	for _, e := range a.IPv4 {
		if e == ip || e == ip+"/32" {
			return true
		}
	}
	return false
}

// WithIP returns a copy of the ACL with ip added to the IPv4 set, enforced and
// sorted+deduped (matching the action's `+ [$ip] | unique`). changed is false
// when ip was already present, so the caller skips the PUT and records no change.
// Adding an allowed IP implies enforcing the ACL, so Enabled is set true; callers
// must short-circuit a disabled (open-to-all) ACL before calling this.
func (a ControlPlaneACL) WithIP(ip string) (ControlPlaneACL, bool) {
	if a.ContainsIP(ip) {
		return a, false
	}
	merged := append(append([]string{}, a.IPv4...), ip)
	sort.Strings(merged)
	return ControlPlaneACL{Enabled: true, IPv4: collapseAdjacentInPlace(merged), IPv6: a.IPv6}, true
}

// WithoutIP returns a copy of the ACL with ip removed from the IPv4 set (both
// bare and /32 forms), preserving Enabled and the IPv6 set. changed is false when
// ip was absent.
func (a ControlPlaneACL) WithoutIP(ip string) (ControlPlaneACL, bool) {
	out := make([]string, 0, len(a.IPv4))
	for _, e := range a.IPv4 {
		if e == ip || e == ip+"/32" {
			continue
		}
		out = append(out, e)
	}
	if len(out) == len(a.IPv4) {
		return a, false
	}
	return ControlPlaneACL{Enabled: a.Enabled, IPv4: out, IPv6: a.IPv6}, true
}

// ListClusters returns every LKE cluster on the account (all pages). Each map
// carries id, label, region, k8s_version and status.
func (c *Client) ListClusters(ctx context.Context) ([]map[string]any, error) {
	return c.listAllPages(ctx, "/v4beta/lke/clusters")
}

// MatchingClusters returns the clusters whose label == label and, when
// region != "", whose region matches too — the resolve-by-label(+region) the
// lke-runner-acl action did with jq. The caller branches on len: 0 none,
// 1 unique, >1 ambiguous.
//
// IT IS THE ONE PLACE THAT DECIDES WHICH CLUSTER BELONGS TO A DEPLOYMENT, and it
// is extracted rather than copied for the reason #443 is entirely about: the
// preflight, `llz doctor` and now `llz env add` must not be able to reach different
// conclusions about one spec. This predicate had already been written twice — here
// and in lke_versions.go — before anyone noticed, and the second copy was added by
// a change whose own comment claimed there was only one.
func MatchingClusters(clusters []map[string]any, label, region string) []map[string]any {
	var out []map[string]any
	for _, m := range clusters {
		if mString(m, "label") != label {
			continue
		}
		if region != "" && mString(m, "region") != region {
			continue
		}
		out = append(out, m)
	}
	return out
}

// MatchClusterIDs is MatchingClusters projected onto ids. Nil for no match, so a
// caller ranging over it sees nothing rather than a zero id.
func MatchClusterIDs(clusters []map[string]any, label, region string) []uint64 {
	var ids []uint64
	for _, m := range MatchingClusters(clusters, label, region) {
		ids = append(ids, mUint(m, "id"))
	}
	return ids
}

// NonNil returns a non-nil empty slice when s is nil so it marshals to a JSON
// array ([]) rather than null — required by the Linode control-plane ACL API.
func NonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// collapseAdjacentInPlace removes adjacent duplicates from an ALREADY-SORTED
// slice, reusing its backing array (out := in[:0]) — so it also clobbers the
// caller's slice contents.
//
// It was named dedupeSorted, matching a function in cmd/llz (import.go) whose
// contract is the opposite: that one is map-based, tolerates unsorted input, and
// allocates rather than mutating. Two different behaviors behind one name is a
// trap for whoever reaches for the "wrong" one — this name states both
// preconditions it actually has.
func collapseAdjacentInPlace(in []string) []string {
	out := in[:0]
	var prev string
	for i, s := range in {
		if i == 0 || s != prev {
			out = append(out, s)
		}
		prev = s
	}
	return out
}
