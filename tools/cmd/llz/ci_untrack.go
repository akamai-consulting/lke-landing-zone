package main

// ci_untrack.go implements `llz ci tf-untrack` — the native port of
// untrack-cluster-bootstrap-on-destroy.sh: before a cluster-bootstrap destroy,
// drop the resources that would otherwise hang the teardown. The CASE A/B
// decision logic + address sets live in internal/terraform; this file is the
// terraform console / API-probe / state-rm orchestration.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
	"github.com/spf13/cobra"
)

func ciTFUntrackCmd() *cobra.Command {
	var destroyClusterToo string
	c := &cobra.Command{
		Use:   "tf-untrack",
		Short: "drop cluster-bootstrap resources from TF state before a destroy (CASE A/B)",
		Long: "Native port of untrack-cluster-bootstrap-on-destroy.sh. Run from the\n" +
			"cluster-bootstrap TF dir after `terraform init`. If the LKE cluster is alive\n" +
			"(CASE A) it drops only the apl-core chain so `helm uninstall` doesn't hang on\n" +
			"finalizers; if the cluster is gone or a hung-create zombie (CASE B) it drops\n" +
			"every cluster-backed resource so the destroy doesn't refresh them to an i/o\n" +
			"timeout. --destroy-cluster-too=1 forces CASE B (the cluster is destroyed in\n" +
			"this same teardown). Idempotent: an address not in state is skipped.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCITFUntrack(gopts, destroyClusterToo == "1")
		},
	}
	c.Flags().StringVar(&destroyClusterToo, "destroy-cluster-too", "0", "\"1\" when the LKE cluster is destroyed in this same teardown (forces CASE B)")
	return c
}

func runCITFUntrack(g globalOpts, destroyClusterToo bool) error {
	// providers.tf normalizes local.kubeconfig_raw to "" (or a present-but-null
	// prints as `null`) when the cluster workspace has no kubeconfig. A console
	// failure falls through to the conservative CASE A path.
	kubeconfigRaw, ok := tfConsole("local.kubeconfig_raw")
	if !ok {
		kubeconfigRaw = "__CONSOLE_FAILED__"
	}
	stateList := tfStateList()

	dropAll := true
	switch {
	case destroyClusterToo:
		fmt.Println("--destroy-cluster-too=1 — the LKE cluster is destroyed in this same teardown; its deletion reaps every in-cluster resource (CASE B, forced).")
	case tf.NoUsableKubeconfig(kubeconfigRaw):
		fmt.Println("Cluster remote state has no usable kubeconfig (empty/null) — cluster gone or never initialized (CASE B).")
	case !clusterAPIReachable():
		fmt.Println("Cluster has a kubeconfig but its API server is unreachable after retries — hung-create zombie / dead control plane (CASE B).")
	default:
		dropAll = false
	}

	if dropAll {
		addrs := tf.ClusterBackedAddrs(stateList)
		if len(addrs) == 0 {
			fmt.Println("no cluster-backed resources in state — nothing to drop.")
			return nil
		}
		fmt.Println("Dropping ALL cluster-backed resources from state (reaped by the cluster delete / unreachable API).")
		return stateRm(g, stateList, addrs...)
	}
	fmt.Println("Cluster reachable and staying — dropping only the apl-core chain so helm uninstall doesn't hang (CASE A).")
	return stateRm(g, stateList, tf.AplCoreChain()...)
}

// tfConsole evaluates a single terraform console expression, returning its
// trimmed stdout. ok is false if terraform console fails (callers treat that
// conservatively — never wipe state out from under a possibly-live cluster).
func tfConsole(expr string) (out string, ok bool) {
	cmd := exec.Command("terraform", "console")
	cmd.Stdin = strings.NewReader(expr + "\n")
	b, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// tfStateList returns `terraform state list` output, or "" on error (an empty
// state — every state rm then becomes a skip).
func tfStateList() string {
	out, err := exec.Command("terraform", "state", "list").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// clusterAPIReachable probes the cluster API /livez. Conservative by design:
// returns true (stay CASE A) on ANY HTTP/TLS response (200/401/403 all count —
// we only care that the endpoint answers) and whenever the host can't be
// determined; returns false only after the endpoint fails every probe or it's
// the providers.tf no-kubeconfig sentinel.
func clusterAPIReachable() bool {
	host, ok := tfConsole("local.kube_host")
	if !ok {
		return true
	}
	host = strings.Trim(host, `"`)
	switch tf.ClassifyKubeHost(host) {
	case tf.KubeHostGone:
		return false
	case tf.KubeHostUnknown:
		return true
	}
	// curl -sk: skip TLS verification — the cluster serves a self-signed cert and
	// we only need to know the endpoint answers, not trust it.
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
	}
	for i := 0; i < 3; i++ {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, host+"/livez", nil)
		if err != nil {
			return true // unparseable host — conservative
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			return true
		}
	}
	return false
}

// stateRm rm's each addr present in stateList (skip-if-absent, idempotent);
// honors --dry-run.
func stateRm(g globalOpts, stateList string, addrs ...string) error {
	for _, addr := range addrs {
		if !tf.StateHas(stateList, addr) {
			fmt.Printf("not in state (skip): %s\n", addr)
			continue
		}
		fmt.Printf("state rm: %s\n", addr)
		if g.dryRun {
			fmt.Fprintln(os.Stderr, "→ (dry-run) terraform state rm "+addr)
			continue
		}
		if err := runTF("state", "rm", addr); err != nil {
			return fmt.Errorf("state rm %s: %w", addr, err)
		}
	}
	return nil
}
