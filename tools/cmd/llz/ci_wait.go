package main

// ci_wait.go implements `llz ci wait-pods`, `llz ci wait-secret` and `llz ci
// wait-cluster-ready` — native ports of the inline kubectl polling loops the
// bootstrap/rotation workflows used to carry (llz-bootstrap-openbao.yml's pod
// wait, llz-bootstrap-dns.yml's Secret wait, llz-secret-rotation.yml's
// post-rotation health gate). One place owns the deadline/interval mechanics
// and the timeout diagnostics instead of each workflow re-rolling them in bash.

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
)

func ciWaitPodsCmd() *cobra.Command {
	var ns, phase string
	var timeout, interval int
	c := &cobra.Command{
		Use:   "wait-pods <pod>...",
		Short: "wait for named pods to reach a status phase (default Running)",
		Long: "Native port of the 'Wait for OpenBao pods to be running' loop in\n" +
			"llz-bootstrap-openbao.yml. Watches each named pod with `kubectl wait`\n" +
			"(--for=create to ride out a not-yet-created pod, then\n" +
			"--for=jsonpath={.status.phase}=<phase>), under one shared --timeout. Phase\n" +
			"(not Readiness) on purpose: a pod can stay unready until a later step acts on\n" +
			"it (OpenBao pods are unready until unsealed), so a readiness wait would\n" +
			"deadlock a first bootstrap. On timeout it dumps the namespace's workloads,\n" +
			"the stuck pod's describe, and recent events (combined stdout+stderr, so an\n" +
			"empty namespace or a NotFound still surfaces the reason), then exits 1.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, pods []string) error {
			os.Exit(runCIWaitPods(ns, phase, pods, timeout, interval))
			return nil
		},
	}
	c.Flags().StringVar(&ns, "namespace", "", "namespace of the pods (required)")
	c.Flags().StringVar(&phase, "phase", "Running", "status phase to wait for")
	c.Flags().IntVar(&timeout, "timeout", 600, "total wait budget in seconds, shared across all pods")
	c.Flags().IntVar(&interval, "interval", 5, "seconds between polls")
	return c
}

func ciWaitSecretCmd() *cobra.Command {
	var ns, name, es string
	var timeout, interval, esTimeout int
	c := &cobra.Command{
		Use:   "wait-secret",
		Short: "wait for a K8s Secret to materialize (optionally until its ExternalSecret is Ready)",
		Long: "Native port of the 'Wait for cert-manager-dns01-solver-token Secret' loop in\n" +
			"llz-bootstrap-dns.yml. Waits for the Secret with `kubectl wait --for=create`\n" +
			"(kubectl 1.31+ — a bare --for=condition errors immediately on NotFound, which\n" +
			"is why this used to be a hand-rolled existence poll) — then, with\n" +
			"--externalsecret, waits for that ExternalSecret's Ready condition (ESO\n" +
			"reports Ready=True only after the Secret has the expected keys). On timeout\n" +
			"it dumps the ExternalSecret's status conditions and exits 1.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			os.Exit(runCIWaitSecret(ns, name, es, timeout, interval, esTimeout))
			return nil
		},
	}
	c.Flags().StringVar(&ns, "namespace", "", "namespace of the Secret (required)")
	c.Flags().StringVar(&name, "name", "", "Secret name to wait for (required)")
	c.Flags().StringVar(&es, "externalsecret", "", "ExternalSecret to require Ready once the Secret exists (empty skips)")
	c.Flags().IntVar(&timeout, "timeout", 180, "seconds to wait for the Secret to exist")
	c.Flags().IntVar(&interval, "interval", 5, "seconds between polls")
	c.Flags().IntVar(&esTimeout, "es-timeout", 60, "seconds to wait for the ExternalSecret Ready condition")
	return c
}

func ciWaitClusterReadyCmd() *cobra.Command {
	var timeout, interval, requestTimeout, expectNodes int
	var tfvarsPath string
	c := &cobra.Command{
		Use:   "wait-cluster-ready",
		Short: "wait until the apiserver answers AND the expected node count is Ready under $KUBECONFIG",
		Long: "Native port of the post-rotation health gate loop in llz-secret-rotation.yml\n" +
			"and the 'Wait for cluster API ready' loop in llz-terraform.yml. Polls\n" +
			"`kubectl get nodes` until (a) the control plane accepts the credentials and\n" +
			"(b) at least --expect-nodes nodes report Ready=True. A fresh LKE pool is\n" +
			"created in seconds (the API returns) but its nodes take minutes to register\n" +
			"and go Ready; gating only on apiserver reachability lets bootstrap proceed\n" +
			"onto an empty pool, where the apl-operator pod (and then helm_release.apl)\n" +
			"sits Pending until it times out. Requiring nodes Ready closes that gap.\n" +
			"With --tfvars the expected count is read from that file's node_count\n" +
			"(overriding --expect-nodes when > 0; autoscaler/absent → falls back to\n" +
			"--expect-nodes). On timeout it dumps node readiness and probes the\n" +
			"apiserver's /version directly so 'API never came up', 'API up but ACL\n" +
			"blocks this runner', and 'API up, nodes never joined' are distinguishable.\n" +
			"Exit 0 ready, 1 on timeout.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			os.Exit(runCIWaitClusterReady(timeout, interval, requestTimeout, resolveExpectNodes(tfvarsPath, expectNodes)))
			return nil
		},
	}
	c.Flags().IntVar(&timeout, "timeout", 360, "total wait budget in seconds")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between polls")
	c.Flags().IntVar(&requestTimeout, "request-timeout", 10, "kubectl --request-timeout per poll, in seconds (bounds a hanging apiserver)")
	c.Flags().IntVar(&expectNodes, "expect-nodes", 1, "minimum number of Ready nodes to wait for")
	c.Flags().StringVar(&tfvarsPath, "tfvars", "", "cluster <region>.tfvars path; its node_count (when > 0) sets the expected Ready-node count")
	return c
}

// resolveExpectNodes returns the number of Ready nodes wait-cluster-ready should
// require. A tfvars file's node_count wins when present and positive (so the gate
// waits for the whole statically-sized pool); otherwise fallback (the
// --expect-nodes flag) applies. The fallback is floored at 1 — a cluster with
// zero Ready nodes is never "ready". Reading tfvars is best-effort: an unreadable
// path silently falls back, matching the gate's tolerance of partial cluster state.
func resolveExpectNodes(tfvarsPath string, fallback int) int {
	if fallback < 1 {
		fallback = 1
	}
	if tfvarsPath == "" {
		return fallback
	}
	content, err := os.ReadFile(tfvarsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::could not read %s (%v) — expecting %d Ready node(s)\n", tfvarsPath, err, fallback)
		return fallback
	}
	if n := tf.ParseTFVars(string(content)).NodeCount; n > 0 {
		return n
	}
	return fallback
}

// waitPoll calls cond until it returns true or timeout elapses, sleeping
// interval between tries; the first try is immediate. Returns whether cond
// succeeded within the budget.
func waitPoll(timeout, interval time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

func runCIWaitPods(ns, phase string, pods []string, timeout, interval int) int {
	_ = interval // kubectl wait is watch-based; --interval retained for CLI compatibility
	if ns == "" {
		fmt.Fprintln(os.Stderr, "::error::--namespace is required")
		return 1
	}
	// One shared deadline across all pods, like the bash loop: a StatefulSet's
	// OrderedReady pods come up one at a time, so per-pod budgets would either
	// be too tight for pod 0 or balloon the worst case.
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for _, pod := range pods {
		// Two watch-based waits under the shared deadline: --for=create rides out a
		// pod that does not exist yet (a bare --for=jsonpath errors on NotFound),
		// then --for=jsonpath blocks until .status.phase matches.
		if err := kubectlWaitStream("-n", ns, "wait", "--for=create", "pod/"+pod,
			fmt.Sprintf("--timeout=%ds", remainingSecs(deadline))); err != nil {
			fmt.Fprintf(os.Stderr, "::error::%s was not created within %ds\n", pod, timeout)
			dumpPodDiagnostics(ns, pod)
			return 1
		}
		if err := kubectlWaitStream("-n", ns, "wait", "--for=jsonpath={.status.phase}="+phase,
			"pod/"+pod, fmt.Sprintf("--timeout=%ds", remainingSecs(deadline))); err != nil {
			fmt.Fprintf(os.Stderr, "::error::%s did not reach %s phase within %ds\n", pod, phase, timeout)
			dumpPodDiagnostics(ns, pod)
			return 1
		}
		fmt.Printf("%s is %s\n", pod, phase)
	}
	return 0
}

// remainingSecs is the whole seconds left until deadline, floored at 1 so a
// `kubectl wait --timeout` is always positive — passing 0 would mean "wait
// forever", the opposite of an exhausted budget.
func remainingSecs(deadline time.Time) int {
	if s := int(time.Until(deadline).Seconds()); s > 1 {
		return s
	}
	return 1
}

// dumpPodDiagnostics prints best-effort cluster state after a wait timeout. It
// goes through execCombined (combined stdout+stderr, unconditional), so the cases
// that previously went dark now surface: an empty namespace prints "No resources
// found" — the signature of a StatefulSet/pods that were never created (e.g. an
// Argo app that hasn't synced) — and a NotFound describe prints its error instead
// of being silently skipped. Events are the highest-signal source: FailedScheduling,
// ImagePull, PVC-binding, and sync failures all land there. Each block is tail-
// capped so a noisy namespace can't bury the output.
func dumpPodDiagnostics(ns, pod string) {
	for _, args := range [][]string{
		{"-n", ns, "get", "statefulset,pod", "-o", "wide"},
		{"-n", ns, "describe", "pod", pod},
		{"-n", ns, "get", "events", "--sort-by=.lastTimestamp"},
	} {
		fmt.Fprintf(os.Stderr, "\n# kubectl %s\n%s\n",
			strings.Join(args, " "), tailLines(execCombined("kubectl", args...), 40))
	}
}

func runCIWaitSecret(ns, name, es string, timeout, interval, esTimeout int) int {
	_ = interval // kubectl wait is watch-based; --interval retained for CLI compatibility
	if ns == "" || name == "" {
		fmt.Fprintln(os.Stderr, "::error::--namespace and --name are required")
		return 1
	}
	// --for=create (kubectl 1.31+) rides out the Secret not existing yet; a bare
	// --for=condition errors immediately on NotFound, which is why this used to be
	// a hand-rolled existence poll.
	if err := kubectlWaitStream("-n", ns, "wait", "--for=create", "secret/"+name,
		fmt.Sprintf("--timeout=%ds", timeout)); err != nil {
		hint := ""
		if es != "" {
			hint = fmt.Sprintf(" — check ExternalSecret status: kubectl -n %s describe externalsecret %s", ns, es)
		}
		fmt.Fprintf(os.Stderr, "::error::%s/%s did not appear within %ds%s\n", ns, name, timeout, hint)
		if es != "" {
			if out, err := execOutput("kubectl", "-n", ns, "get", "externalsecret", es, "-o", "jsonpath={.status.conditions}"); err == nil {
				os.Stderr.Write(append(out, '\n'))
			}
		}
		return 1
	}
	if es != "" {
		if err := kubectlWaitStream("-n", ns, "wait", "--for=condition=Ready",
			"externalsecret/"+es, fmt.Sprintf("--timeout=%ds", esTimeout)); err != nil {
			fmt.Fprintf(os.Stderr, "::error::externalsecret/%s in %s did not become Ready within %ds\n", es, ns, esTimeout)
			return 1
		}
		fmt.Printf("%s/%s is present and ExternalSecret %s Ready.\n", ns, name, es)
		return 0
	}
	fmt.Printf("%s/%s is present.\n", ns, name)
	return 0
}

func runCIWaitClusterReady(timeout, interval, requestTimeout, expectNodes int) int {
	if expectNodes < 1 {
		expectNodes = 1
	}
	var apiReachable bool
	ok := waitPoll(time.Duration(timeout)*time.Second, time.Duration(interval)*time.Second, func() bool {
		// jsonpath: one "<node>=<Ready-condition-status>" line per node, so a
		// reachable-but-empty pool prints nothing and parses to 0 Ready.
		out, err := execOutput("kubectl", "get", "nodes",
			"-o", `jsonpath={range .items[*]}{.metadata.name}{"="}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}`,
			fmt.Sprintf("--request-timeout=%ds", requestTimeout))
		if err != nil {
			fmt.Println("Waiting for the control plane to accept the kubeconfig...")
			return false
		}
		apiReachable = true
		ready := countReadyNodes(string(out))
		if ready < expectNodes {
			fmt.Printf("Control plane reachable; %d/%d node(s) Ready — waiting for the pool to come up...\n", ready, expectNodes)
			return false
		}
		return true
	})
	if !ok {
		if apiReachable {
			fmt.Fprintf(os.Stderr, "::error::control plane is reachable but fewer than %d node(s) became Ready within %ds — the node pool never came up (check Linode capacity/quota for the requested type, or a node_pool_label ≥ 16 chars).\n", expectNodes, timeout)
			fmt.Fprintf(os.Stderr, "\n# kubectl get nodes -o wide\n%s\n", tailLines(execCombined("kubectl", "get", "nodes", "-o", "wide"), 40))
		} else {
			fmt.Fprintf(os.Stderr, "::error::cluster unreachable after %ds — investigate the kubeconfig before relying on CI.\n", timeout)
		}
		diagnoseAPIServer()
		return 1
	}
	fmt.Printf("Control plane is reachable and ≥%d node(s) Ready:\n", expectNodes)
	fmt.Print(execCombined("kubectl", "get", "nodes", "-o", "wide"))
	return 0
}

// countReadyNodes counts nodes reporting Ready=True from the wait loop's
// jsonpath output — one "<node>=<status>" line per node (status is the Ready
// condition's value, e.g. "True"/"False"/"Unknown", or empty if the condition
// is not yet present). Pure + tested: it is the gate's core decision.
func countReadyNodes(jsonpathOut string) int {
	ready := 0
	for _, line := range strings.Split(jsonpathOut, "\n") {
		_, status, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && status == "True" {
			ready++
		}
	}
	return ready
}

// diagnoseAPIServer probes the kubeconfig's server URL directly on timeout:
// /version answering while `kubectl get nodes` fails points at the credentials
// or the control-plane ACL (is this runner's egress IP opened?), not at a
// still-provisioning API. Best-effort — no kubeconfig, no probe.
func diagnoseAPIServer() {
	out, err := execOutput("kubectl", "config", "view", "--minify",
		"-o", "jsonpath={.clusters[0].cluster.server}")
	server := strings.TrimSpace(string(out))
	if err != nil || server == "" {
		fmt.Fprintln(os.Stderr, "API endpoint: unknown (could not read the kubeconfig server)")
		return
	}
	fmt.Fprintf(os.Stderr, "API endpoint: %s\n", server)
	resp, perr := apiProbeClient().Get(server + "/version")
	if perr != nil {
		fmt.Fprintf(os.Stderr, "direct /version probe failed: %v (API not reachable from this runner — still provisioning, or the control-plane ACL does not include this runner's egress IP)\n", perr)
		return
	}
	defer resp.Body.Close()
	body := make([]byte, 300)
	n, _ := resp.Body.Read(body)
	fmt.Fprintf(os.Stderr, "direct /version probe: HTTP %d %s (API is up — suspect the kubeconfig credentials)\n",
		resp.StatusCode, strings.TrimSpace(string(body[:n])))
}

// apiProbeClient is the short-deadline, verification-off HTTP client the
// timeout diagnostics use (LKE's API cert is fine; a bootstrap-window probe
// must not fail on TLS while the endpoint is mid-provision). A package var so
// tests can point it at a stub server.
var apiProbeClient = func() *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
	}
}

// kubectlWaitStream runs `kubectl <args>` with streamed output (the condition
// wait's progress lines are the operator's feedback). A package var so tests
// can stub the wait.
var kubectlWaitStream = func(args ...string) error {
	cmd := exec.Command("kubectl", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
