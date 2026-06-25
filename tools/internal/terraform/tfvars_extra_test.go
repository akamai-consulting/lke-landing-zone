package terraform

import "testing"

// TestDefaultNodePoolLabelLen guards the constant against the regression that
// shipped an 18-char "observability-pool" default: a node_pool_label of 16+ chars
// has left LKE nodes never joining the pool. The cluster module enforces the same
// bound via a variable validation; this keeps llz's reconstructed default (which
// MUST match that module default) on the legal side of the line too.
func TestDefaultNodePoolLabelLen(t *testing.T) {
	if len(DefaultNodePoolLabel) >= MaxNodePoolLabelLen {
		t.Errorf("DefaultNodePoolLabel %q is %d chars; must be < %d (LKE nodes fail to join with longer labels)",
			DefaultNodePoolLabel, len(DefaultNodePoolLabel), MaxNodePoolLabelLen)
	}
}

// The package's TestParseTFVars only asserts ClusterLabel/NodePoolLabel, so the
// firewall_label parse branch is the one statement ParseTFVars leaves uncovered.
func TestParseTFVarsFirewallLabel(t *testing.T) {
	v := ParseTFVars("cluster_label = \"c1\"\nfirewall_label = \"fw-explicit\"\n")
	if v.FirewallLabel != "fw-explicit" {
		t.Errorf("FirewallLabel = %q, want fw-explicit", v.FirewallLabel)
	}
	// First assignment wins, matching cluster_label/node_pool_label semantics.
	v = ParseTFVars("firewall_label = \"first\"\nfirewall_label = \"second\"\n")
	if v.FirewallLabel != "first" {
		t.Errorf("FirewallLabel = %q, want first (first-wins)", v.FirewallLabel)
	}
}

// Region feeds `llz ci runner-acl`'s cluster resolution (the Linode datacenter,
// not the deployment env key).
func TestParseTFVarsRegion(t *testing.T) {
	v := ParseTFVars("cluster_label = \"c1\"\nregion = \"us-ord\"\n")
	if v.Region != "us-ord" {
		t.Errorf("Region = %q, want us-ord", v.Region)
	}
}

func TestParseTFVarsVPCNetwork(t *testing.T) {
	// Shared VPC: vpc_network set → the module's linode_vpc.this is count 0
	// (tf-import must NOT import a `.this[0]` that doesn't exist).
	if v := ParseTFVars("cluster_label = \"c1\"\nvpc_network = \"shared-ord\"\n"); v.VPCNetwork != "shared-ord" {
		t.Errorf("VPCNetwork = %q, want shared-ord", v.VPCNetwork)
	}
	// Dedicated VPC: vpc_network absent/empty → the VPC resource exists at
	// linode_vpc.this[0], the address tf-import must use.
	if v := ParseTFVars("cluster_label = \"c1\"\n"); v.VPCNetwork != "" {
		t.Errorf("VPCNetwork = %q, want empty (dedicated VPC)", v.VPCNetwork)
	}
}
