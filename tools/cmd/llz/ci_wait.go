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
)

func ciWaitPodsCmd() *cobra.Command {
	var ns, phase string
	var timeout, interval int
	c := &cobra.Command{
		Use:   "wait-pods <pod>...",
		Short: "wait for named pods to reach a status phase (default Running)",
		Long: "Native port of the 'Wait for OpenBao pods to be running' loop in\n" +
			"llz-bootstrap-openbao.yml. Polls each named pod's .status.phase until it\n" +
			"matches --phase, under one shared --timeout. Phase (not Readiness) on\n" +
			"purpose: a pod can stay unready until a later step acts on it (OpenBao\n" +
			"pods are unready until unsealed), so a readiness wait would deadlock a\n" +
			"first bootstrap. On timeout it dumps the namespace's workloads, the stuck\n" +
			"pod's describe, and recent events (combined stdout+stderr, so an empty\n" +
			"namespace or a NotFound still surfaces the reason), then exits 1.",
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
			"llz-bootstrap-dns.yml. Polls Secret existence first — a bare `kubectl wait\n" +
			"--for=condition` errors immediately on NotFound — then, with\n" +
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
	var timeout, interval, requestTimeout int
	c := &cobra.Command{
		Use:   "wait-cluster-ready",
		Short: "wait until the apiserver answers `kubectl get nodes` under $KUBECONFIG",
		Long: "Native port of the post-rotation health gate loop in llz-secret-rotation.yml\n" +
			"and the 'Wait for cluster API ready' loop in llz-terraform.yml. Polls\n" +
			"`kubectl get nodes` until the control plane accepts the credentials — a\n" +
			"fresh cluster's API takes minutes to provision, and after an\n" +
			"lke-admin-token rotation the regenerated kubeconfig takes time to sync —\n" +
			"and prints the node list on success. On timeout it probes the apiserver's\n" +
			"/version endpoint directly so 'API never came up' and 'API up but rejecting\n" +
			"this kubeconfig / ACL blocks this runner' are distinguishable. Exit 0\n" +
			"reachable, 1 on timeout.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			os.Exit(runCIWaitClusterReady(timeout, interval, requestTimeout))
			return nil
		},
	}
	c.Flags().IntVar(&timeout, "timeout", 360, "total wait budget in seconds")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between polls")
	c.Flags().IntVar(&requestTimeout, "request-timeout", 10, "kubectl --request-timeout per poll, in seconds (bounds a hanging apiserver)")
	return c
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
	if ns == "" {
		fmt.Fprintln(os.Stderr, "::error::--namespace is required")
		return 1
	}
	// One shared deadline across all pods, like the bash loop: a StatefulSet's
	// OrderedReady pods come up one at a time, so per-pod budgets would either
	// be too tight for pod 0 or balloon the worst case.
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	for _, pod := range pods {
		ok := waitPoll(time.Until(deadline), time.Duration(interval)*time.Second, func() bool {
			out, err := execOutput("kubectl", "-n", ns, "get", "pod", pod, "-o", "jsonpath={.status.phase}")
			return err == nil && strings.TrimSpace(string(out)) == phase
		})
		if !ok {
			fmt.Fprintf(os.Stderr, "::error::%s did not reach %s phase within %ds\n", pod, phase, timeout)
			dumpPodDiagnostics(ns, pod)
			return 1
		}
		fmt.Printf("%s is %s\n", pod, phase)
	}
	return 0
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
	if ns == "" || name == "" {
		fmt.Fprintln(os.Stderr, "::error::--namespace and --name are required")
		return 1
	}
	start := time.Now()
	ok := waitPoll(time.Duration(timeout)*time.Second, time.Duration(interval)*time.Second, func() bool {
		if kExists("-n", ns, "get", "secret", name) {
			return true
		}
		fmt.Printf("waiting for %s/%s (%ds/%ds)...\n", ns, name, int(time.Since(start).Seconds()), timeout)
		return false
	})
	if !ok {
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

func runCIWaitClusterReady(timeout, interval, requestTimeout int) int {
	var nodes []byte
	ok := waitPoll(time.Duration(timeout)*time.Second, time.Duration(interval)*time.Second, func() bool {
		out, err := execOutput("kubectl", "get", "nodes",
			fmt.Sprintf("--request-timeout=%ds", requestTimeout))
		if err != nil {
			fmt.Println("Waiting for the control plane to accept the kubeconfig...")
			return false
		}
		nodes = out
		return true
	})
	if !ok {
		fmt.Fprintf(os.Stderr, "::error::cluster unreachable after %ds — investigate the kubeconfig before relying on CI.\n", timeout)
		diagnoseAPIServer()
		return 1
	}
	fmt.Println("Control plane is reachable:")
	os.Stdout.Write(nodes)
	return 0
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
