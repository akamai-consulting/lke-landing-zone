package main

// status_preflight.go — turn "no cluster access" into instructions instead of a
// wall of kubectl noise.
//
// `llz status <env>` is the quickstart's last step, and the step most likely to
// be run with no way to reach the cluster: the cluster was built by GitHub
// Actions, so the operator's laptop has no kubeconfig for it unless they went and
// fetched one. Every check status runs is a kubectl call, so that state produced
// three copies of kubectl's five-line memcache dump, one connection-refused per
// check, and exit 1 — 18 lines that never mention a kubeconfig.
//
// The information the operator needs is fixed and short (fetch a kubeconfig,
// point KUBECONFIG at it, and be inside the control-plane ACL), so probe once and
// print that instead.

import (
	"fmt"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/color"
)

// clusterReachable probes the cluster kubectl currently points at with a short
// timeout, returning kubectl's own diagnosis on failure.
var clusterReachable = func() (string, bool) {
	_, err := execOutput("kubectl", "version", "-o", "json", "--request-timeout=8s")
	if err == nil {
		return "", true
	}
	// The diagnosis is kubectl's own; keep its first line, which is the useful
	// one ("connection refused", "Unauthorized", "i/o timeout").
	return firstLine(err.Error()), false
}

// statusPreflight fails with the fix when the current context cannot reach a
// cluster, so `llz status` never runs its checks blind.
func statusPreflight(env string) error {
	if !lookable("kubectl") {
		return fmt.Errorf("kubectl is not on PATH — `llz status` reads the cluster with it (`llz doctor` lists the tooling)")
	}
	if ctx := toolOut("kubectl", "config", "current-context"); ctx == "" {
		return noClusterAccessErr(env, "no current kubectl context is set")
	}
	if why, ok := clusterReachable(); !ok {
		return noClusterAccessErr(env, why)
	}
	return nil
}

// toolOut is execOutput reduced to trimmed stdout, "" on any error. (gitOut
// does the same for git; this is the tool-agnostic form.)
func toolOut(name string, args ...string) string {
	out, err := execOutput(name, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// noClusterAccessErr renders the one remediation an operator needs here: the
// cluster is built by CI, so getting a kubeconfig is a deliberate step, and on
// LKE-E reaching it also requires being inside the control-plane ACL.
func noClusterAccessErr(env, why string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "no reachable cluster for the current kubectl context (%s).\n", why)
	b.WriteString("  The build runs in GitHub Actions, so nothing has written a kubeconfig here yet. Fetch one:\n")
	fmt.Fprintf(&b, "      %s\n", color.Cyan("export LINODE_API_TOKEN=$(grep ^LINODE_API_TOKEN .llz/secrets.env | cut -d= -f2-)"))
	fmt.Fprintf(&b, "      %s\n", color.Cyan(fmt.Sprintf("llz ci fetch-kubeconfig --region %s --output ~/.kube/%s.yaml", env, env)))
	fmt.Fprintf(&b, "      %s\n", color.Cyan(fmt.Sprintf("export KUBECONFIG=~/.kube/%s.yaml", env)))
	// fetch-kubeconfig resolves the cluster from <env>.tfvars, and those are
	// gitignored build artifacts — absent in a fresh clone until a render.
	fmt.Fprintf(&b, "  %s\n", color.Dim(fmt.Sprintf("(fresh clone? `llz render %s` first — it resolves the cluster from the rendered %s.tfvars)", env, env)))
	// The ACL is enabled unconditionally by the cluster module, and its address set
	// is exactly cluster.apiServerAllowCIDRs. The quickstart's default answer for
	// that field is EMPTY (correct for github.com-hosted runners, which open their
	// egress IP per job and revoke it on the way out), so the common case here is
	// not a misconfigured ACL — it is a correctly-configured one that has never
	// contained this laptop. "Edit the spec and re-apply" was the only remedy named,
	// which costs a full apply to run one kubectl. `runner-acl open` is the same
	// Linode-API ACL write the CI job does, and an operator can run it directly.
	b.WriteString("  Still refused or timing out? The LKE-E control-plane ACL admits only the prefixes in\n")
	b.WriteString("  cluster.apiServerAllowCIDRs, and a github.com-hosted build leaves none of yours there.\n")
	b.WriteString("  Add THIS machine's egress IP to the live ACL — takes effect at once, no re-apply:\n")
	// Needs a token, and runner-acl NO-OPS WITH EXIT 0 when it has none (it is
	// built for a CI job where a missing token should not fail the step), so an
	// operator who skipped the export would otherwise see success and still be
	// refused. --region on BOTH lines: it names the state file `revoke` reads back,
	// and without it revoke looks under "default", finds nothing, and reports a
	// no-op — leaving a home IP in the control-plane ACL indefinitely.
	fmt.Fprintf(&b, "      %s\n", color.Cyan("export LINODE_TOKEN=…   # runner-acl no-ops (exit 0) without it"))
	fmt.Fprintf(&b, "      %s\n", color.Cyan("llz ci runner-acl open --region "+env))
	fmt.Fprintf(&b, "      %s\n", color.Cyan("llz ci runner-acl revoke --region "+env)+color.Dim("   # when you are done"))
	fmt.Fprintf(&b, "  %s\n", color.Dim("(add --runner-configmap if this cluster runs the cidrFirewall component, whose"))
	fmt.Fprintf(&b, "  %s\n", color.Dim("controller replaces the ACL on each reconcile; it is off by default.)"))
	// NOT "edit the spec and re-apply": the cluster resource carries
	// `ignore_changes = [control_plane[0].acl, pool]`, so the ACL is set at CREATE
	// only and a re-apply against a live cluster is a no-op on it. Saying otherwise
	// buys the operator a 20-minute apply that changes nothing and reports success.
	fmt.Fprintf(&b, "  A spec edit (%s) does NOT change this cluster's ACL — Terraform holds it\n", color.Cyan("llz env edit "+env))
	b.WriteString("  under ignore_changes, so it applies only to a cluster created later. Or run from an allowed host.")
	return fmt.Errorf("%s", b.String())
}
