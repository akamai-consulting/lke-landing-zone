package terraform

import "testing"

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

// A real connection-level blip against the Linode API mid-apply — the class this
// heal absorbs now that no root dials the LKE-E apiserver.
const linodeAPIFlakeLog = `linode_lke_cluster.this: Still creating... [10m0s elapsed]

Error: Error waiting for LKE Cluster to finish creating: Get "https://api.linode.com/v4beta/lke/clusters/622766": net/http: TLS handshake timeout

  with linode_lke_cluster.this,
  on main.tf line 12, in resource "linode_lke_cluster" "this":
  12: resource "linode_lke_cluster" "this" {
`

const linodeAPIEOFLog = `Error: [Get "https://api.linode.com/v4/networking/firewalls/12345": EOF]
`

func TestTransientAPIFlake(t *testing.T) {
	if !TransientAPIFlake(linodeAPIFlakeLog) {
		t.Error("should detect a TLS handshake timeout against api.linode.com")
	}
	if !TransientAPIFlake(linodeAPIEOFLog) {
		t.Error("should detect a bare EOF against api.linode.com")
	}
	// A connection error that does NOT name the Linode API must not be treated as
	// a transient blip — that narrowness is what keeps a genuine resource error
	// from being silently retried.
	if TransientAPIFlake(`Error: dial tcp 10.0.0.5:443: connect: connection refused`) {
		t.Error("false positive on a connection error to a non-API endpoint")
	}
	// The retired anchor: the LKE-E apiserver. Nothing in the surviving roots
	// (cluster, object-storage, vpc) dials it — a136aa5 deleted the
	// cluster-bootstrap workspace that did — so a log naming it is not a class we
	// can still produce, and matching it would only widen the retry surface.
	if TransientAPIFlake(`Error: Kubernetes cluster unreachable: Get "https://lke621819.api.us-ord.enterprise.linodelke.net:6443/version": net/http: TLS handshake timeout`) {
		t.Error("must not match the retired LKE-E apiserver anchor")
	}
	// Genuine resource-level failures must NOT be treated as transient.
	if TransientAPIFlake(fwCollisionLog) {
		t.Error("false positive on a firewall collision")
	}
	if TransientAPIFlake("Apply complete!") {
		t.Error("false positive on a clean apply")
	}
}

// fwDeviceReadLog is the read-after-write consistency flake that burned a cold
// e2e create (run 29655607246): the node firewall was created but the provider's
// device read-back failed, with terraform's generic invalid-result diagnostic.
const fwDeviceReadLog = `module.cluster.module.node_firewall.linode_firewall.this.linodes. All values
Error: Provider returned invalid result object after apply
Error: Failed to Get Devices for Firewall 76987661
  with module.cluster.module.node_firewall.linode_firewall.this,
  on .terraform/modules/cluster/terraform-modules/llz-node-firewall/main.tf line 5, in resource "linode_firewall" "this":
   5: resource "linode_firewall" "this" {`

func TestFirewallDeviceReadFlake(t *testing.T) {
	if !FirewallDeviceReadFlake(fwDeviceReadLog) {
		t.Error("should detect the 'Failed to Get Devices for Firewall' read-after-write flake")
	}
	// Must NOT be confused with the create-collision heal (that one imports; this
	// one just retries) or a clean apply.
	if FirewallDeviceReadFlake(fwCollisionLog) {
		t.Error("false positive on a firewall label collision (that is Heal B, not a device-read retry)")
	}
	if FirewallDeviceReadFlake("Apply complete! Resources: 6 added.") {
		t.Error("false positive on a clean apply")
	}
	// The generic 'invalid result object' alone (no firewall device-read) must NOT
	// match — too broad to retry blindly.
	if FirewallDeviceReadFlake("Error: Provider returned invalid result object after apply") {
		t.Error("false positive on the generic invalid-result error without the firewall device-read signature")
	}
}
