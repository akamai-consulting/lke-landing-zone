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
	fmt.Fprintf(&b, "      %s\n", cyan("export LINODE_API_TOKEN=$(grep ^LINODE_API_TOKEN .llz/secrets.env | cut -d= -f2-)"))
	fmt.Fprintf(&b, "      %s\n", cyan(fmt.Sprintf("llz ci fetch-kubeconfig --region %s --output ~/.kube/%s.yaml", env, env)))
	fmt.Fprintf(&b, "      %s\n", cyan(fmt.Sprintf("export KUBECONFIG=~/.kube/%s.yaml", env)))
	// fetch-kubeconfig resolves the cluster from <env>.tfvars, and those are
	// gitignored build artifacts — absent in a fresh clone until a render.
	fmt.Fprintf(&b, "  %s\n", dim(fmt.Sprintf("(fresh clone? `llz render %s` first — it resolves the cluster from the rendered %s.tfvars)", env, env)))
	b.WriteString("  Still refused or timing out? The LKE-E control-plane ACL admits only the prefixes in\n")
	fmt.Fprintf(&b, "  cluster.apiServerAllowCIDRs — add your egress one (%s, then re-apply),\n", cyan("llz env edit "+env))
	b.WriteString("  or run the checks from a host that is already allowed.")
	return fmt.Errorf("%s", b.String())
}
