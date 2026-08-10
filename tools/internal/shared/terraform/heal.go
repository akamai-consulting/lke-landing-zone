package terraform

import (
	"regexp"
	"strings"
)

// The self-heal parsers below read `terraform apply -no-color` output. -no-color
// is load-bearing: terraform's colorized diagnostics prefix the "  with <addr>,"
// lines with ANSI codes and a "│" box that defeat these anchors.

var (
	fwCreateFailRe = regexp.MustCompile(`Failed to Create Firewall`)
	withFirewallRe = regexp.MustCompile(`^\s*with\s+([^\s,]*linode_firewall\.[^\s,]+)`)

	fwDeviceReadRe = regexp.MustCompile(`Failed to Get Devices for Firewall`)
)

// transientAPINetErrors are the connection-level failures that mean "the Linode
// API flaked", not "a resource is wrong". Matched only on a line that also names
// the API endpoint (see TransientAPIFlake) so a genuine resource error is never
// mistaken for a connectivity blip and silently retried.
var transientAPINetErrors = []string{
	"tls handshake timeout",
	"i/o timeout",
	"connection refused",
	"context deadline exceeded",
	"server gave http response to https client",
	": eof",
}

// transientAPIHost anchors the transient-flake match to the Linode API endpoint,
// so a resource-level error that merely happens to contain a net-error substring
// is never retried as a connectivity blip.
//
// This used to anchor on linodelke.net / :6443 — the LKE-E APISERVER — because
// the flake it absorbed came from the cluster-bootstrap workspace's
// data.kubernetes_service / helm provider. Commit a136aa5 deleted that workspace,
// and nothing in the remaining roots (cluster, object-storage, vpc) dials the
// apiserver, so that anchor could no longer match anything. The roots DO talk to
// api.linode.com throughout a 20-30 minute cluster apply, and transient TLS/5xx
// blips there are a real class (Heal D exists because one burned a cold e2e), so
// the mechanism is retargeted rather than deleted.
const transientAPIHost = "api.linode.com"

// FirewallCollisionMsg is the Linode error that signals two Cloud Firewalls would
// share a label (labels are account-unique).
const FirewallCollisionMsg = "Label must be unique among your Cloud Firewalls"

// FirewallCollision reports whether the apply failed because a Cloud Firewall
// label already exists (Heal B's trigger).
func FirewallCollision(applyLog string) bool {
	return strings.Contains(applyLog, FirewallCollisionMsg)
}

// TransientAPIFlake reports whether a terraform plan/apply failed only because
// the kubernetes/helm provider could not reach the LKE-E apiserver — the HA
// control plane can drop an individual replica moments after wait-cluster-ready
// passed, killing an otherwise-valid run. There is no TF state to repair; the fix
// is to let the control plane settle and retry.
//
// Two shapes are caught: the provider's own "Kubernetes cluster unreachable"
// wording (apply), and a bare data-source/provider call that failed with a
// connection-level error AGAINST the cluster API — e.g. the plan-time
// `data.kubernetes_service.coredns` read:
//
//	Error: Get "https://lke…linodelke.net:6443/api/v1/…": net/http: TLS handshake timeout
//
// Deliberately narrow: a net-error line must ALSO name the API endpoint
// (linodelke.net or :6443) so a genuine resource-level timeout elsewhere is not
// mistaken for a control-plane blip and silently retried.
func TransientAPIFlake(log string) bool {
	for _, line := range strings.Split(log, "\n") {
		l := strings.ToLower(line)
		if !strings.Contains(l, transientAPIHost) {
			continue
		}
		for _, e := range transientAPINetErrors {
			if strings.Contains(l, e) {
				return true
			}
		}
	}
	return false
}

// FirewallDeviceReadFlake reports whether the apply failed only because the
// Linode provider could not read a Cloud Firewall's attached devices
// (linodes/nodebalancers) immediately after creating it — a read-after-write
// consistency flake. The provider surfaces it as
//
//	Error: Failed to Get Devices for Firewall 76987661
//
// usually alongside terraform core's generic "Provider returned invalid result
// object after apply" on the same module.cluster…linode_firewall.this. There is
// NO state to repair: the firewall exists; the device read just needs a moment
// to settle, after which a re-plan + re-apply succeeds. Narrow by construction —
// only this specific device-read error is matched, so a genuine firewall
// misconfiguration (quota, invalid rule) still fails fast rather than being
// silently retried. This class of transient burned a whole cold e2e create
// (run 29655607246), which is why it is retriable.
func FirewallDeviceReadFlake(log string) bool {
	return fwDeviceReadRe.MatchString(log)
}

// ParseFirewallAddress returns the linode_firewall resource address whose create
// failed with the label collision (Heal B). "" if it cannot be parsed.
func ParseFirewallAddress(applyLog string) string {
	saw := false
	for _, line := range strings.Split(applyLog, "\n") {
		if fwCreateFailRe.MatchString(line) {
			saw = true
		}
		if saw {
			if m := withFirewallRe.FindStringSubmatch(line); m != nil {
				return m[1]
			}
		}
	}
	return ""
}
