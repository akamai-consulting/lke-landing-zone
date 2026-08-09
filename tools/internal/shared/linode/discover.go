package linode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// discover.go — instance-anchored lookups for in-cluster self-discovery: a pod
// that knows only its own Node can walk providerID → instance → attached
// firewall / owning LKE cluster / VPC subnet, replacing config that CI used to
// resolve out-of-band (from tfvars + account-wide label scans) and seed in.

// InstanceFirewalls returns the Cloud Firewalls attached to a Linode instance
// (all pages). Linode caps Cloud Firewalls at one per linode, so for an LKE
// node this is expected to contain exactly the node-pool firewall.
func (c *Client) InstanceFirewalls(ctx context.Context, linodeID uint64) ([]map[string]any, error) {
	return c.listAllPages(ctx, fmt.Sprintf("/v4/linode/instances/%d/firewalls", linodeID))
}

// InstanceConfigs returns an instance's configuration profiles (all pages).
func (c *Client) InstanceConfigs(ctx context.Context, linodeID uint64) ([]map[string]any, error) {
	return c.listAllPages(ctx, fmt.Sprintf("/v4/linode/instances/%d/configs", linodeID))
}

// InstanceLKEClusterID returns the id of the LKE cluster the instance belongs
// to (the instance object's `lke_cluster_id`), or 0 when the field is absent /
// null (not an LKE node, or an API version that predates the field — callers
// fall back to parsing the node name).
func (c *Client) InstanceLKEClusterID(ctx context.Context, linodeID uint64) (uint64, error) {
	resp, err := c.get(ctx, "v4", fmt.Sprintf("linode/instances/%d", linodeID))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("GET linode/instances/%d returned %d (check the PAT scope)", linodeID, resp.StatusCode)
	}
	var body map[string]any
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return 0, fmt.Errorf("parsing instance %d: %w", linodeID, err)
	}
	return mUint(body, "lke_cluster_id"), nil
}

// VPCInterface returns the vpc_id + subnet_id of the first `purpose: vpc`
// interface across an instance's config profiles; ok=false when the instance
// has no VPC interface (e.g. a cluster provisioned without a VPC).
func VPCInterface(configs []map[string]any) (vpcID, subnetID uint64, ok bool) {
	for _, cfg := range configs {
		ifaces, _ := cfg["interfaces"].([]any)
		for _, raw := range ifaces {
			iface, isMap := raw.(map[string]any)
			if !isMap || mString(iface, "purpose") != "vpc" {
				continue
			}
			return mUint(iface, "vpc_id"), mUint(iface, "subnet_id"), true
		}
	}
	return 0, 0, false
}

// SubnetIPv4 returns the ipv4 CIDR of the subnet with the given id from a
// ListVPCSubnets collection; ok=false when the id is not present.
func SubnetIPv4(subnets []map[string]any, subnetID uint64) (string, bool) {
	for _, s := range subnets {
		if mUint(s, "id") == subnetID {
			return mString(s, "ipv4"), true
		}
	}
	return "", false
}

// LinodeIDFromProviderID parses a Kubernetes Node.spec.providerID of the form
// `linode://12345` (the Linode CCM convention on LKE and LKE-E); ok=false for
// any other shape.
func LinodeIDFromProviderID(providerID string) (uint64, bool) {
	rest, found := strings.CutPrefix(providerID, "linode://")
	if !found || rest == "" || !isAllDigits(rest) {
		return 0, false
	}
	var id uint64
	for _, r := range rest {
		id = id*10 + uint64(r-'0')
	}
	return id, true
}

// LKEClusterIDFromNodeName parses the cluster id from an LKE node name
// (`lke<cluster>-<pool>-<suffix>`, e.g. lke393244-59879-0a1b2c3d); "" when the
// name does not match. Fallback for instances whose API object lacks
// lke_cluster_id.
func LKEClusterIDFromNodeName(name string) string {
	rest, found := strings.CutPrefix(name, "lke")
	if !found {
		return ""
	}
	digits, _, hasDash := strings.Cut(rest, "-")
	if !hasDash || digits == "" || !isAllDigits(digits) {
		return ""
	}
	return digits
}
