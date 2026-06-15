package terraform

import "testing"

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
