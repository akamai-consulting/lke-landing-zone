// Package terraform holds the pure decision logic ported out of the
// instance-scripts/terraform/*.sh CI steps: tfvars parsing, the cluster-label
// derivations, node-pool selection, `terraform state show` id extraction, and
// the kubeconfig-or-stub decision. It is deliberately side-effect free (no exec,
// no HTTP, no filesystem) so every branch the bash drifted on is unit-tested;
// the `llz ci` orchestrator in cmd/llz wires it to terraform + the Linode client.
package terraform

import (
	"strconv"
	"strings"
)

// DefaultNodePoolLabel is the fallback node-pool label when <region>.tfvars sets
// none — identical to terraform-linode-import.sh's `${NODE_POOL_LABEL:-...}`.
const DefaultNodePoolLabel = "observability-pool"

// TFVars holds the handful of values the CI terraform steps read out of a
// <region>.tfvars file. FirewallLabel is the raw firewall_label override (""
// when unset); use ResolveFirewallLabel for the effective label. NodeType /
// NodeCount feed the preflight vCPU-quota estimate (NodeCount is 0 when absent).
type TFVars struct {
	ClusterLabel  string
	NodePoolLabel string
	FirewallLabel string
	NodeType      string
	NodeCount     int
	// Region is the Linode datacenter region (e.g. us-ord) — distinct from the
	// deployment/env key. It disambiguates clusters that share a label across
	// envs (used by `llz ci runner-acl` to resolve the cluster).
	Region string
}

// ParseTFVars extracts the cluster labels + node_type/node_count from tfvars
// content, mirroring the scripts' `grep '^key' | sed 's/.*= *"\(.*\)"/\1/'`: the
// first assignment of each key wins, surrounding quotes are stripped, and a
// missing node_pool_label falls back to DefaultNodePoolLabel.
func ParseTFVars(content string) TFVars {
	var v TFVars
	for _, line := range strings.Split(content, "\n") {
		key, val, ok := splitAssign(line)
		if !ok {
			continue
		}
		switch key {
		case "cluster_label":
			if v.ClusterLabel == "" {
				v.ClusterLabel = val
			}
		case "node_pool_label":
			if v.NodePoolLabel == "" {
				v.NodePoolLabel = val
			}
		case "firewall_label":
			if v.FirewallLabel == "" {
				v.FirewallLabel = val
			}
		case "region":
			if v.Region == "" {
				v.Region = val
			}
		case "node_type":
			if v.NodeType == "" {
				v.NodeType = val
			}
		case "node_count":
			if v.NodeCount == 0 {
				if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
					v.NodeCount = n
				}
			}
		}
	}
	if v.NodePoolLabel == "" {
		v.NodePoolLabel = DefaultNodePoolLabel
	}
	return v
}

// splitAssign parses a `key = "value"` line into its key and unquoted value.
// Leading whitespace and a trailing comment are ignored; ok is false for lines
// without an `=`.
func splitAssign(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, '=')
	if i < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:i])
	val = strings.TrimSpace(line[i+1:])
	// Strip surrounding double quotes if present (tfvars strings are quoted).
	if len(val) >= 2 && val[0] == '"' {
		if j := strings.IndexByte(val[1:], '"'); j >= 0 {
			val = val[1 : 1+j]
		}
	}
	return key, val, true
}

// Labels are the derived Linode resource labels the import step searches for.
type Labels struct {
	Cluster  string
	NodePool string
	VPC      string
	Subnet   string
	Firewall string
}

// clusterLabelTrunc is the substr length the llz-cluster module applies to
// cluster_label when deriving the default firewall label (see ResolveFirewallLabel).
const clusterLabelTrunc = 26

// ResolveFirewallLabel returns the effective Cloud Firewall label, matching the
// llz-cluster module exactly (terraform-modules/llz-cluster/main.tf):
//
//	var.firewall_label != "" ? var.firewall_label : "${substr(cluster_label,0,26)}-nodes"
//
// NOTE: this is the authoritative derivation. The retired terraform-linode-import.sh
// used "(cluster+\"-nodes\")[:32]" with no override — wrong for cluster labels
// longer than 26 chars or whenever firewall_label is set; that mismatch is exactly
// what let an orphan firewall slip past the import step into apply's create-collision.
func ResolveFirewallLabel(v TFVars) string {
	if v.FirewallLabel != "" {
		return v.FirewallLabel
	}
	c := v.ClusterLabel
	if len(c) > clusterLabelTrunc {
		c = c[:clusterLabelTrunc]
	}
	return c + "-nodes"
}

// DeriveLabels reproduces the cluster resource labels the import step searches
// for: <cluster>-vpc, <cluster>-nodes (subnet), and the module-correct node
// firewall label (ResolveFirewallLabel).
func DeriveLabels(v TFVars) Labels {
	return Labels{
		Cluster:  v.ClusterLabel,
		NodePool: v.NodePoolLabel,
		VPC:      v.ClusterLabel + "-vpc",
		Subnet:   v.ClusterLabel + "-nodes",
		Firewall: ResolveFirewallLabel(v),
	}
}
