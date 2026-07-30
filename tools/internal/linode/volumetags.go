package linode

// volumetags.go holds the tag-heal primitives for `llz ci reconcile-volume-tags`
// — the in-cluster backstop that re-stamps StorageClass volumeTags onto any
// Volume born without them (the one known path: clone/snapshot PVCs admitted
// during a Kyverno webhook outage; the Linode CloneVolume API takes no tags).
// This is a NARROW resurrection of the retired volume-labeler's API primitives:
// tags only — no label rewriting (Volumes keep their pvc-<uuid> labels; renaming
// them breaks reap's `pvc-` candidate filter), and no node-instance lookup (the
// desired tag set, lke<id> included, is read from the live StorageClass — the
// single source of truth cluster-bootstrap renders).

import (
	"context"
	"encoding/json"
	"fmt"
)

// MergeTags returns tags with every missing desired tag appended (deduped,
// order-stable: existing first, then missing desired in their given order) and
// whether anything changed. Linode replaces a Volume's whole tag list on update,
// so the caller PUTs this full set to add tags without dropping existing ones.
func MergeTags(tags, desired []string) ([]string, bool) {
	have := make(map[string]bool, len(tags))
	for _, t := range tags {
		have[t] = true
	}
	out := tags
	changed := false
	for _, d := range desired {
		if d == "" || have[d] {
			continue
		}
		if !changed {
			// Copy-on-first-write so the caller's slice is never mutated.
			out = append(make([]string, 0, len(tags)+len(desired)), tags...)
			changed = true
		}
		out = append(out, d)
		have[d] = true
	}
	return out, changed
}

// Volume returns one Volume by id and the HTTP status; (nil, 404, nil) when the
// Volume is gone (deleted out-of-band but a PV still references it).
func (c *Client) Volume(ctx context.Context, id string) (map[string]any, int, error) {
	return c.getOne(ctx, "v4", "volumes/"+id)
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
// list wholesale, so callers pass the complete desired set (see MergeTags).
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
