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
	"errors"
	"fmt"
	"os"
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
	// Wrong directory, before wrong cluster. Every remediation below is written for
	// someone standing in their instance — it opens with `grep … .llz/secrets.env`
	// and goes on to `llz ci fetch-kubeconfig`, which resolves the cluster through
	// <env>.tfvars. Run from anywhere else, all of that is noise about a file that
	// is not there. `llz env add` already refuses this case precisely; status
	// printed the fifteen-line kubeconfig block instead.
	if err := requireStatusInstanceRoot(env); err != nil {
		return err
	}
	if ctx := toolOut("kubectl", "config", "current-context"); ctx == "" {
		return noClusterAccessErr(env, "no current kubectl context is set")
	}
	if why, ok := clusterReachable(); !ok {
		return noClusterAccessErr(env, why)
	}
	return nil
}

// requireStatusInstanceRoot refuses `llz status` outside an instance checkout.
//
// Separate wording from requireInstanceRoot's, because the reason differs: env
// add would AUTHOR files in the wrong place, whereas status would merely read the
// wrong cluster — but every remedy it prints (the `.llz/secrets.env` token, the
// <env>.tfvars fetch-kubeconfig resolves the cluster through) is relative to the
// instance root, so away from it the whole block is unfollowable.
func requireStatusInstanceRoot(env string) error {
	if isInstanceRoot(".") {
		return nil
	}
	cwd, _ := os.Getwd()
	var b strings.Builder
	fmt.Fprintf(&b, "`llz status` must run from your instance repo root, and %s is not one.\n", cwd)
	b.WriteString("  It resolves the cluster through that deployment's rendered tfvars and reads the\n")
	b.WriteString("  Linode token from .llz/secrets.env — neither of which exists here.\n")
	switch cands := instanceSubdirs("."); {
	case len(cands) > 0:
		b.WriteString("  • your instance is right here — cd into it first:\n")
		for _, c := range cands {
			fmt.Fprintf(&b, "      %s\n", cyan("cd "+c+" && llz status "+env))
		}
	case enclosingInstanceRoot() != "":
		fmt.Fprintf(&b, "  • you are inside an instance, below its root — go up to it:\n      %s\n",
			cyan("cd "+enclosingInstanceRoot()+" && llz status "+env))
	default:
		fmt.Fprintf(&b, "  • already scaffolded one?  %s\n", cyan("cd <instance-dir> && llz status "+env))
		fmt.Fprintf(&b, "  • checking a cluster you already have a kubeconfig for? %s\n",
			cyan("kubectl get applications -A -n argocd"))
	}
	return errors.New(strings.TrimRight(b.String(), "\n"))
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
	fmt.Fprintf(&b, "      %s\n", cyan("export LINODE_TOKEN=…   # runner-acl no-ops (exit 0) without it"))
	fmt.Fprintf(&b, "      %s\n", cyan("llz ci runner-acl open --region "+env))
	fmt.Fprintf(&b, "      %s\n", cyan("llz ci runner-acl revoke --region "+env)+dim("   # when you are done"))
	fmt.Fprintf(&b, "  %s\n", dim("(add --runner-configmap if this cluster runs the cidrFirewall component, whose"))
	fmt.Fprintf(&b, "  %s\n", dim("controller replaces the ACL on each reconcile; it is off by default.)"))
	// NOT "edit the spec and re-apply": the cluster resource carries
	// `ignore_changes = [control_plane[0].acl, pool]`, so the ACL is set at CREATE
	// only and a re-apply against a live cluster is a no-op on it. Saying otherwise
	// buys the operator a 20-minute apply that changes nothing and reports success.
	fmt.Fprintf(&b, "  A spec edit (%s) does NOT change this cluster's ACL — Terraform holds it\n", cyan("llz env edit "+env))
	b.WriteString("  under ignore_changes, so it applies only to a cluster created later. Or run from an allowed host.")
	return fmt.Errorf("%s", b.String())
}
