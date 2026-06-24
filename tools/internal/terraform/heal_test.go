package terraform

import "testing"

// -no-color apply output: no ╷│╵ box, plain 2-space-indented "with" lines (the
// exact reason terraform-apply-with-heal mandates -no-color).
const helmPhantomLog = `module.cluster_bootstrap.helm_release.apl: Destroying...

Error: uninstall: Release not loaded: apl: release: not found

  with helm_release.apl,
  on main.tf line 10, in resource "helm_release" "apl":
  10: resource "helm_release" "apl" {
`

func TestParseHelmPhantom(t *testing.T) {
	if got := ParseHelmPhantom(helmPhantomLog); got != "helm_release.apl" {
		t.Errorf("ParseHelmPhantom = %q, want helm_release.apl", got)
	}
	// No matching error => no address.
	if got := ParseHelmPhantom("Apply complete!"); got != "" {
		t.Errorf("ParseHelmPhantom(clean) = %q, want empty", got)
	}
	// A `with helm_release.` line that is NOT preceded by the not-found error
	// must not be picked up (the `saw` gate).
	if got := ParseHelmPhantom("  with helm_release.other,\n"); got != "" {
		t.Errorf("ParseHelmPhantom(no error preamble) = %q, want empty", got)
	}
}

const fwCollisionLog = `Error: Failed to Create Firewall

  with module.cluster.module.node_firewall.linode_firewall.this,
  on ../../terraform-modules/llz-node-firewall/main.tf line 1:

[400] [label] Label must be unique among your Cloud Firewalls
`

func TestFirewallCollisionAndAddress(t *testing.T) {
	if !FirewallCollision(fwCollisionLog) {
		t.Error("FirewallCollision should detect the unique-label error")
	}
	if FirewallCollision("some other apply error") {
		t.Error("FirewallCollision false positive")
	}
	got := ParseFirewallAddress(fwCollisionLog)
	want := "module.cluster.module.node_firewall.linode_firewall.this"
	if got != want {
		t.Errorf("ParseFirewallAddress = %q, want %q", got, want)
	}
	if got := ParseFirewallAddress("no firewall failure here"); got != "" {
		t.Errorf("ParseFirewallAddress(none) = %q, want empty", got)
	}
}

const clusterUnreachableLog = `helm_release.apl: Still creating... [10s elapsed]

Error: Kubernetes cluster unreachable: Get "https://lke621819.api.us-ord.enterprise.linodelke.net:6443/version": net/http: TLS handshake timeout

  with helm_release.apl,
  on main.tf line 357, in resource "helm_release" "apl":
 357: resource "helm_release" "apl" {
`

func TestTransientClusterUnreachable(t *testing.T) {
	if !TransientClusterUnreachable(clusterUnreachableLog) {
		t.Error("TransientClusterUnreachable should detect the cluster-unreachable error")
	}
	// A genuine resource-level failure must NOT be treated as transient.
	if TransientClusterUnreachable(fwCollisionLog) {
		t.Error("TransientClusterUnreachable false positive on a firewall collision")
	}
	if TransientClusterUnreachable("Apply complete!") {
		t.Error("TransientClusterUnreachable false positive on a clean apply")
	}
}
