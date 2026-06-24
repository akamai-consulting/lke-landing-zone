package terraform

import (
	"regexp"
	"strings"
)

// The self-heal parsers below read `terraform apply -no-color` output. -no-color
// is load-bearing: terraform's colorized diagnostics prefix the "  with <addr>,"
// lines with ANSI codes and a "│" box that defeat these anchors.

var (
	helmNotFoundRe = regexp.MustCompile(`Error:.*release.*not found`)
	withHelmRe     = regexp.MustCompile(`^\s*with\s+(helm_release\.[^\s,]+)`)

	fwCreateFailRe = regexp.MustCompile(`Failed to Create Firewall`)
	withFirewallRe = regexp.MustCompile(`^\s*with\s+([^\s,]*linode_firewall\.[^\s,]+)`)

	clusterUnreachableRe = regexp.MustCompile(`Error:.*[Kk]ubernetes cluster unreachable`)
)

// FirewallCollisionMsg is the Linode error that signals two Cloud Firewalls would
// share a label (labels are account-unique).
const FirewallCollisionMsg = "Label must be unique among your Cloud Firewalls"

// ParseHelmPhantom returns the address of a helm_release whose apply failed with
// "release: not found" — a phantom left in TF state after the cluster lost the
// release (Heal A). "" if the log shows no such failure.
func ParseHelmPhantom(applyLog string) string {
	saw := false
	for _, line := range strings.Split(applyLog, "\n") {
		if helmNotFoundRe.MatchString(line) {
			saw = true
		}
		if saw {
			if m := withHelmRe.FindStringSubmatch(line); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// FirewallCollision reports whether the apply failed because a Cloud Firewall
// label already exists (Heal B's trigger).
func FirewallCollision(applyLog string) bool {
	return strings.Contains(applyLog, FirewallCollisionMsg)
}

// TransientClusterUnreachable reports whether the apply failed only because the
// kubernetes/helm provider could not reach the API server ("Kubernetes cluster
// unreachable" — a TLS handshake / i/o timeout against :6443). The LKE-E HA
// control plane can flake on an individual apiserver replica moments after
// wait-cluster-ready passed, killing an otherwise-valid apply (Heal C's
// trigger). There is no TF state to repair — the fix is to let the control plane
// settle and retry. Intentionally narrow: it anchors on the provider's exact
// "cluster unreachable" wording so a genuine resource-level failure is not
// mistaken for a connectivity blip and silently retried.
func TransientClusterUnreachable(applyLog string) bool {
	return clusterUnreachableRe.MatchString(applyLog)
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
