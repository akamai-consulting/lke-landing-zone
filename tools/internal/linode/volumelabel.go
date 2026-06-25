package linode

// volumelabel.go ports the volume-labeler's relabel.sh (a ConfigMap shell script
// the labeler CronJob ran) into tested Go: the label/tag DECISION logic as pure
// functions here, plus the one-Volume / one-Instance API primitives the `llz ci
// label-volumes` driver calls. Keeping the decisions pure is the whole point of
// the move — the bash was untestable embedded-in-YAML logic.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ── pure decision logic ──────────────────────────────────────────────────────

// DesiredVolumeLabel builds the human-readable Linode Volume label
// `<regionShort>-<namespace>-<pvc>`: sanitized to Linode's [A-Za-z0-9_-] charset
// (every other byte -> '-'), truncated to the 32-char label cap, with any
// trailing '-' left by truncation stripped. Mirrors the old relabel.sh
// `tr -c 'A-Za-z0-9_-' '-' | cut -c -32 | sed 's/-*$//'`.
func DesiredVolumeLabel(regionShort, namespace, pvc string) string {
	raw := regionShort + "-" + namespace + "-" + pvc
	b := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
			b = append(b, c)
		default:
			b = append(b, '-')
		}
	}
	if len(b) > 32 {
		b = b[:32]
	}
	return strings.TrimRight(string(b), "-")
}

// ClusterTagForVolume returns the `lke<id>` ownership tag to stamp on a cluster's
// PVCs, derived from one of its node instances' tags — LKEIDFromTags handles the
// CCM's `lke<id>` / `lke-<id>` forms and normalises to `lke<id>`. "" when the
// node carries no cluster-id tag.
func ClusterTagForVolume(nodeInstanceTags []string) string {
	if id := LKEIDFromTags(nodeInstanceTags); id != "" {
		return "lke" + id
	}
	return ""
}

// MergeClusterTag returns tags with clusterTag appended when missing (deduped,
// order-stable) and whether it changed. Linode replaces a Volume's whole tag list
// on update, so the caller PUTs this full set to add the cluster tag without
// dropping the CSI-applied ones. A "" clusterTag is a no-op.
func MergeClusterTag(tags []string, clusterTag string) ([]string, bool) {
	if clusterTag == "" {
		return tags, false
	}
	for _, t := range tags {
		if t == clusterTag {
			return tags, false
		}
	}
	out := make([]string, len(tags), len(tags)+1)
	copy(out, tags)
	return append(out, clusterTag), true
}

// InstanceIDFromProviderID extracts the Linode instance id from a Kubernetes
// node's spec.providerID ("linode://12345" -> "12345"); "" if not a Linode id.
func InstanceIDFromProviderID(providerID string) string {
	const p = "linode://"
	if !strings.HasPrefix(providerID, p) {
		return ""
	}
	return strings.TrimPrefix(providerID, p)
}

// ── API primitives ───────────────────────────────────────────────────────────

// Volume returns one Volume by id and the HTTP status; (nil, 404, nil) when the
// Volume is gone (deleted out-of-band but a PV still references it).
func (c *Client) Volume(ctx context.Context, id string) (map[string]any, int, error) {
	return c.getOne(ctx, "v4", "volumes/"+id)
}

// Instance returns one Linode instance by id and the HTTP status — used to read a
// node's `lke<id>` tag for cluster attribution.
func (c *Client) Instance(ctx context.Context, id string) (map[string]any, int, error) {
	return c.getOne(ctx, "v4", "linode/instances/"+id)
}

// getOne GETs a single Linode resource (not a `data`-wrapped collection) and
// returns it as a generic map with json.Number preserved. A non-2xx yields
// (nil, status, nil) so the caller can branch (e.g. on 404).
func (c *Client) getOne(ctx context.Context, version, path string) (map[string]any, int, error) {
	resp, err := c.get(ctx, version, path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, nil
	}
	var m map[string]any
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parsing %s response: %w", path, err)
	}
	return m, resp.StatusCode, nil
}

// UpdateVolume PUTs a Volume's label and full tag set. Linode replaces the tag
// list wholesale, so callers pass the complete desired set (see MergeClusterTag).
// Returns the HTTP status.
func (c *Client) UpdateVolume(ctx context.Context, id, label string, tags []string) (int, error) {
	resp, err := c.put(ctx, fmt.Sprintf("%s/v4/volumes/%s", c.base, id), map[string]any{
		"label": label,
		"tags":  tags,
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("PUT volume %s returned %d: %s", id, resp.StatusCode, readBody(resp))
	}
	return resp.StatusCode, nil
}
