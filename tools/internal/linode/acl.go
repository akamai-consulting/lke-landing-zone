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
	"sort"
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
			Enabled   *bool `json:"enabled"`
			Addresses struct {
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
	return ControlPlaneACL{Enabled: enabled, IPv4: body.ACL.Addresses.IPv4, IPv6: body.ACL.Addresses.IPv6}, nil
}

// PutControlPlaneACL replaces the cluster's control-plane ACL. Address lists are
// forced non-nil so they marshal as [] rather than null.
func (c *Client) PutControlPlaneACL(ctx context.Context, clusterID uint64, acl ControlPlaneACL) error {
	body := map[string]any{
		"acl": map[string]any{
			"enabled":   acl.Enabled,
			"addresses": map[string]any{"ipv4": NonNil(acl.IPv4), "ipv6": NonNil(acl.IPv6)},
		},
	}
	url := fmt.Sprintf("%s/v4beta/lke/clusters/%d/control_plane_acl", c.base, clusterID)
	resp, err := c.put(ctx, url, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("PUT control-plane ACL for cluster %d returned %d: %s",
			clusterID, resp.StatusCode, readBody(resp))
	}
	return nil
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

// MatchClusterIDs returns the ids of clusters whose label == label and, when
// region != "", whose region matches too — the resolve-by-label(+region) the
// lke-runner-acl action did with jq. The caller branches on len: 0 none,
// 1 unique, >1 ambiguous.
func MatchClusterIDs(clusters []map[string]any, label, region string) []uint64 {
	var ids []uint64
	for _, m := range clusters {
		if mString(m, "label") != label {
			continue
		}
		if region != "" && mString(m, "region") != region {
			continue
		}
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
