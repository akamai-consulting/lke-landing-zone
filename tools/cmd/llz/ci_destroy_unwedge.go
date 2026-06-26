package main

// ci_destroy_unwedge.go implements `llz ci destroy-unwedge` — the native port of
// null_resource.unwedge_namespace_finalizers_on_destroy's local-exec heredoc in
// instance-template cluster-bootstrap/main.tf. It runs as a `when = destroy`
// provisioner BEFORE `helm uninstall apl`, while the cluster still serves the
// kubeconfig, and proactively clears the finalizer/discovery deadlocks that
// otherwise make the apl uninstall's `--wait` run out its full timeout and ERROR:
//
//   1. Argo CD deadlock — helm removes the argocd controller, but ~60+
//      Applications/AppProjects still carry resources-finalizer.argocd.argoproj.io
//      with nothing left to process them, so the argocd namespace never finalizes.
//   2. Broken aggregated discovery — an APIService whose backing Service is gone
//      reports Available=False, failing namespace-deletion discovery cluster-wide
//      (NamespaceDeletionDiscoveryFailure).
//   3. Operator-managed CRs holding finalizers — e.g. CNPG Postgres Clusters/
//      Poolers (the Harbor/Keycloak/Gitea back ends) keep cnpg.io finalizers after
//      their operator is removed, hanging those namespaces in Terminating.
//
// Everything is BEST-EFFORT and non-fatal (exit 0 always): if the cluster is
// already unreachable or a call fails, we continue — the subsequent DESTROY
// Cluster job deletes the LKE cluster and reaps all of this regardless. The value
// is a clean, fast `terraform destroy` instead of a 10-15m hang ending in error.
//
// SECURITY: the kubeconfig arrives base64-encoded in $KUBECONFIG_B64 (an
// environment var), NEVER interpolated into the command string — Terraform echoes
// the rendered `command` to its log but not the `environment` block, so inlining
// the kubeconfig (with its live bearer token) would leak it on every destroy.
//
// The state machine is driven through an injected kubectl seam so it is
// unit-tested without a cluster; the stale-APIService selection is a pure function.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// kubectlRunner runs one kubectl invocation (KUBECONFIG already wired by the
// caller) and returns its combined output plus whether it exited 0.
type kubectlRunner func(args ...string) (string, bool)

func ciDestroyUnwedgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "destroy-unwedge",
		Short: "clear Argo/discovery/CNPG finalizer deadlocks before helm uninstalls apl (destroy-time)",
		Long: "Native port of null_resource.unwedge_namespace_finalizers_on_destroy's\n" +
			"local-exec heredoc. Runs as a destroy-time provisioner while the cluster is\n" +
			"still up and clears the wedges that otherwise make `helm uninstall apl`'s\n" +
			"--wait time out: scales down Argo CD, strips resources-finalizer.argocd from\n" +
			"Applications/AppProjects, deletes stale aggregated APIServices (dead backing\n" +
			"Service → Available=False → discovery failure), and strips CNPG cluster/pooler\n" +
			"finalizers. All best-effort and non-fatal (always exit 0); a subsequent\n" +
			"cluster delete reaps everything regardless. Reads the kubeconfig from the\n" +
			"$KUBECONFIG_B64 environment var (base64), never from argv.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIDestroyUnwedge() },
	}
}

func runCIDestroyUnwedge() error {
	b64 := os.Getenv("KUBECONFIG_B64")
	if b64 == "" {
		return fmt.Errorf("KUBECONFIG_B64 must be set")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("KUBECONFIG_B64 is not valid base64: %w", err)
	}
	kubeconfig, err := os.CreateTemp("", "llz-unwedge-kubeconfig-*")
	if err != nil {
		return fmt.Errorf("create kubeconfig tempfile: %w", err)
	}
	defer os.Remove(kubeconfig.Name())
	if _, err := kubeconfig.Write(raw); err != nil {
		kubeconfig.Close()
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	kubeconfig.Close()

	kubectl := func(args ...string) (string, bool) {
		cmd := exec.Command("kubectl", args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig.Name())
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		return buf.String(), cmd.Run() == nil
	}
	return destroyUnwedge(kubectl)
}

// argoFinalizerKinds carry resources-finalizer.argocd.argoproj.io; cnpgFinalizerKinds
// carry cnpg.io finalizers. Both are stripped cluster-wide so their namespaces
// can finalize. Add other finalizer-bearing kinds here if a future destroy stalls.
var (
	argoFinalizerKinds = []string{"applications.argoproj.io", "appprojects.argoproj.io"}
	cnpgFinalizerKinds = []string{"clusters.postgresql.cnpg.io", "poolers.postgresql.cnpg.io"}
)

func destroyUnwedge(kubectl kubectlRunner) error {
	// If the cluster is already gone (orphaned state, re-run after a prior
	// destroy) there is nothing to unwedge — exit cleanly.
	if _, ok := kubectl("get", "--raw=/healthz", "--request-timeout=15s"); !ok {
		fmt.Println("Cluster API unreachable — skipping finalizer unwedge (already torn down).")
		return nil
	}

	// Phase 1 — stop Argo CD reconciliation BEFORE stripping finalizers, else the
	// app-of-apps re-syncs and re-applies the finalizer to everything cleared below.
	fmt.Println("=== Unwedge phase 1: stop Argo CD reconciliation ===")
	kubectl("-n", "argocd", "scale", "statefulset/argocd-application-controller", "--replicas=0", "--request-timeout=30s")
	kubectl("-n", "argocd", "scale", "deployment/argocd-applicationset-controller", "--replicas=0", "--request-timeout=30s")

	// Phase 2 — strip Argo finalizers, then delete the CRs (their managed children
	// are reaped by the cluster delete). -A: child apps may live outside argocd.
	fmt.Println("=== Unwedge phase 2: strip finalizers from Argo Applications + AppProjects ===")
	for _, kind := range argoFinalizerKinds {
		stripFinalizers(kubectl, kind)
		kubectl("delete", kind, "-A", "--all", "--wait=false", "--request-timeout=30s")
	}

	// Phase 3 — delete stale aggregated APIServices (dead backing Service →
	// Available=False → NamespaceDeletionDiscoveryFailure). Built-in groups (no
	// .spec.service) are never touched.
	fmt.Println("=== Unwedge phase 3: delete stale aggregated APIServices ===")
	if out, ok := kubectl("get", "apiservices", "-o", "json", "--request-timeout=30s"); ok {
		for _, name := range staleAggregatedAPIServices(out) {
			if _, ok := kubectl("delete", "apiservice", name, "--wait=false", "--request-timeout=30s"); ok {
				fmt.Printf("  deleted stale APIService %s\n", name)
			}
		}
	}

	// Phase 4 — strip operator-managed CR finalizers that block namespace GC after
	// their controller is gone (CNPG Postgres Clusters/Poolers). CRD-guarded so a
	// cluster that never ran CNPG is a clean no-op.
	fmt.Println("=== Unwedge phase 4: strip finalizers from operator-managed CRs that block namespace GC ===")
	for _, kind := range cnpgFinalizerKinds {
		if _, ok := kubectl("get", "crd", kind, "--request-timeout=15s"); !ok {
			continue
		}
		stripFinalizers(kubectl, kind)
	}

	fmt.Println("Destroy unwedge complete — helm_release.apl uninstall and namespace GC should now proceed without a finalizer deadlock.")
	return nil
}

// stripFinalizers lists every <kind> across all namespaces and patches its
// finalizers to []. Best-effort: a list/patch failure is logged-by-omission and
// skipped (the cluster delete reaps the resource regardless).
func stripFinalizers(kubectl kubectlRunner, kind string) {
	out, ok := kubectl("get", kind, "-A",
		"-o", `jsonpath={range .items[*]}{.metadata.namespace} {.metadata.name}{"\n"}{end}`,
		"--request-timeout=30s")
	if !ok {
		return
	}
	for _, p := range parseNsNamePairs(out) {
		if _, ok := kubectl("patch", kind, p.name, "-n", p.namespace, "--type=merge",
			"-p", `{"metadata":{"finalizers":[]}}`, "--request-timeout=30s"); ok {
			fmt.Printf("  cleared finalizers on %s %s/%s\n", kind, p.namespace, p.name)
		}
	}
}

type nsName struct{ namespace, name string }

// parseNsNamePairs parses the `{.metadata.namespace} {.metadata.name}` jsonpath
// output (one "ns name" per line). Lines without exactly two fields (e.g. the
// trailing blank, or a name-less row) are skipped — the bash `[ -z "$NAME" ]` guard.
func parseNsNamePairs(out string) []nsName {
	var pairs []nsName
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			pairs = append(pairs, nsName{namespace: f[0], name: f[1]})
		}
	}
	return pairs
}

// staleAggregatedAPIServices parses `kubectl get apiservices -o json` and returns
// the names of AGGREGATED APIServices (those with .spec.service set) currently
// reporting Available=False — the dead-backing-Service entries that stall
// namespace-deletion discovery. Built-in/local groups (no .spec.service) and
// healthy aggregated services are excluded. Mirrors the former jq filter; an
// unparseable payload yields nil (nothing deleted).
func staleAggregatedAPIServices(jsonOut string) []string {
	var doc struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Service *json.RawMessage `json:"service"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &doc); err != nil {
		return nil
	}
	var stale []string
	for _, it := range doc.Items {
		if it.Spec.Service == nil {
			continue // built-in/local group — never touched
		}
		available := ""
		for _, c := range it.Status.Conditions {
			if c.Type == "Available" {
				available = c.Status
			}
		}
		if available == "False" {
			stale = append(stale, it.Metadata.Name)
		}
	}
	return stale
}
