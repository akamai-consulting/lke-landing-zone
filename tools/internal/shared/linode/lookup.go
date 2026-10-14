package linode

import (
	"context"
	"encoding/json"
	"fmt"
)

// FindIDByLabel returns the id of the first resource map whose "label" equals
// label. The caller passes a fully-paginated collection (e.g. from ListVPCs /
// ListFirewalls), so unlike the old `curl ...?page_size=500 | jq` one-shot this
// cannot miss a match that falls past the first page.
func FindIDByLabel(items []map[string]any, label string) (uint64, bool) {
	for _, m := range items {
		if mString(m, "label") == label {
			return mUint(m, "id"), true
		}
	}
	return 0, false
}

// ListNodePools returns the node pools of an LKE cluster (all pages).
func (c *Client) ListNodePools(ctx context.Context, clusterID uint64) ([]map[string]any, error) {
	return c.listAllPages(ctx, fmt.Sprintf("/v4beta/lke/clusters/%d/pools", clusterID))
}

// ListInstances returns all Linode compute instances on the account (all pages).
func (c *Client) ListInstances(ctx context.Context) ([]map[string]any, error) {
	return c.listAllPages(ctx, "/v4/linode/instances")
}

// SumInstanceVCPUs totals the .specs.vcpus across a list of instances — the
// account's in-use vCPU count (all teams, on a shared account).
func SumInstanceVCPUs(instances []map[string]any) int {
	total := 0
	for _, in := range instances {
		if specs, ok := in["specs"].(map[string]any); ok {
			total += int(mUint(specs, "vcpus"))
		}
	}
	return total
}

// LinodeTypeVCPUs returns the vCPU count of a Linode plan type (e.g. g6-standard-4),
// or 0 if the type is unknown / has no vcpus field.
func (c *Client) LinodeTypeVCPUs(ctx context.Context, typeID string) (int, error) {
	resp, err := c.get(ctx, "v4", "linode/types/"+typeID)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, nil
	}
	var body struct {
		VCPUs int `json:"vcpus"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("parsing linode type %s: %w", typeID, err)
	}
	return body.VCPUs, nil
}

// GetKubeconfig returns the base64-encoded kubeconfig for an LKE cluster. A
// non-2xx response yields ("", nil) so the caller writes a stub kubeconfig —
// matching the import script's curl-with-`|| true` tolerance of a transient API
// error or a not-yet-ready cluster.
func (c *Client) GetKubeconfig(ctx context.Context, clusterID uint64) (string, error) {
	resp, err := c.get(ctx, "v4", fmt.Sprintf("lke/clusters/%d/kubeconfig", clusterID))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil
	}
	var body struct {
		Kubeconfig string `json:"kubeconfig"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("parsing kubeconfig response: %w", err)
	}
	return body.Kubeconfig, nil
}
