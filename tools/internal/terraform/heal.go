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
)

// transientAPINetErrors are the connection-level failures that mean "the LKE-E
// apiserver flaked", not "a resource is wrong". Matched only on a line that also
// names the API endpoint (see TransientAPIFlake) so a genuine resource error is
// never mistaken for a connectivity blip and silently retried.
var transientAPINetErrors = []string{
	"tls handshake timeout",
	"i/o timeout",
	"connection refused",
	"context deadline exceeded",
	"server gave http response to https client",
	": eof",
}

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
		if strings.Contains(l, "kubernetes cluster unreachable") {
			return true
		}
		if !strings.Contains(l, "linodelke.net") && !strings.Contains(l, ":6443") {
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
