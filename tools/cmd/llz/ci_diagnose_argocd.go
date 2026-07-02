package main

// ci_diagnose_argocd.go implements `llz ci diagnose-argocd` — the native port
// of llz-terraform.yml's 'Diagnose ArgoCD install failure' step. Runs only on
// the failure path and dumps everything needed to see WHY the bootstrap is not
// becoming ready. Diagnostics must never mask the original failure, so every
// probe is best-effort and the command always exits 0.
//
// The most common failure on a fresh cluster is helm_release.apl hitting its
// 600s wait timeout (context deadline exceeded): the apl-operator Deployment
// never becomes Available, usually because no worker node is Ready/schedulable
// or the operator image can't be pulled. That release lives in the apl-operator
// namespace; the argocd namespace is created later, in-cluster, only once the
// operator's helmfile pipeline gets that far. So we sweep BOTH namespaces —
// apl-operator first (the earlier, more likely failure point), then argocd —
// instead of looking only at an argocd namespace that is empty by design when
// the operator install is what failed.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/spf13/cobra"
)

func ciDiagnoseArgoCDCmd() *cobra.Command {
	var ns, aplNS string
	c := &cobra.Command{
		Use:   "diagnose-argocd",
		Short: "dump apl-operator + ArgoCD install-failure diagnostics (best-effort, never fails)",
		Long: "Native port of the 'Diagnose ArgoCD install failure' step. Dumps node\n" +
			"schedulability, then for the apl-operator and argocd namespaces: their\n" +
			"resources, Jobs + their logs, per-pod describes, recent events, and the\n" +
			"Helm release status/history — grouped with ::group:: for the run log.\n" +
			"apl-operator is swept first because helm_release.apl timing out (operator\n" +
			"Deployment never Available) is the most common fresh-cluster failure, and\n" +
			"the argocd namespace is empty by design until the operator gets that far.\n" +
			"Then sweeps every failing pod / Job across ALL namespaces and dumps its\n" +
			"container logs — the crash reason the state-only captures miss.\n" +
			"Skips cleanly when $KUBECONFIG is absent/empty (cluster may not exist).\n" +
			"Always exits 0: diagnostics must never mask the failure that triggered them.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIDiagnoseArgoCD(aplNS, ns) },
	}
	c.Flags().StringVar(&ns, "namespace", "argocd", "namespace holding the ArgoCD install")
	c.Flags().StringVar(&aplNS, "apl-namespace", "apl-operator", "namespace holding the apl-operator install")
	return c
}

// diagStream runs a command with output streamed to stdout, best-effort. A
// package var so tests can record the probe sequence without real binaries.
var diagStream = func(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	_ = cmd.Run()
}

func runCIDiagnoseArgoCD(aplNS, argoNS string) error {
	kc := os.Getenv("KUBECONFIG")
	if st, err := os.Stat(kc); kc == "" || err != nil || st.Size() == 0 {
		fmt.Fprintln(os.Stderr, "::warning::No kubeconfig available — cluster may not exist; nothing to diagnose")
		return nil
	}

	diagGroup("Nodes (schedulable? Ready?)", func() {
		diagStream("kubectl", "get", "nodes", "-o", "wide")
		// The bash piped describe through grep for the scheduling-relevant
		// sections; print them from the captured describe instead.
		if out, err := execOutput("kubectl", "describe", "nodes"); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				switch {
				case strings.HasPrefix(line, "Name:"), strings.HasPrefix(line, "Taints:"),
					strings.HasPrefix(line, "Conditions:"), strings.HasPrefix(line, "Allocated resources:"):
					fmt.Println(line)
				}
			}
		}
	})

	// Sweep apl-operator first: helm_release.apl ("apl") timing out is the most
	// likely fresh-cluster failure, and argocd is empty until the operator's
	// helmfile pipeline gets that far.
	diagnoseNamespace(aplNS, "apl")
	diagnoseNamespace(argoNS, "argocd")

	// The install can REACH argocd yet still never pass the convergence gate —
	// an Application wedged OutOfSync/Missing (a child's ComparisonError) or the
	// phase gate misreading the OpenBao/cert-manager handoff. Neither shows in the
	// namespace sweeps above, so capture them explicitly before teardown.
	diagnoseConvergence(argoNS)

	// The captures above report STATES; a workload's root cause is a container LOG
	// they never grab. Sweep every failing pod / Job across all namespaces and dump
	// its logs — on a torn-down cluster this is the only record of WHY it failed.
	diagnoseFailingWorkloads()

	fmt.Println("Diagnostics complete. Common causes:")
	fmt.Println("  • apl-operator pod stuck Pending  -> no Ready/schedulable node (check Nodes / Taints / Conditions above)")
	fmt.Println("  • ImagePullBackOff                -> registry unreachable or image pull secret missing")
	fmt.Println("  • CrashLoopBackOff                -> see Job / pod logs above")
	fmt.Println("  • argocd namespace empty          -> apl-operator helmfile pipeline never reached argocd (see apl-operator above)")
	fmt.Println("  • Application OutOfSync/Missing    -> see 'Argo CD Applications' below; a child ComparisonError stalls the parent app-of-apps")
	fmt.Println("  • phase gate stuck                 -> see platform-app-ca plus OpenBao ClusterSecretStore / cert-manager CA chain below")
	return nil
}

// diagnoseConvergence captures the two convergence-gate blockers the namespace
// sweeps miss: the Argo CD Application states (sync/health + the condition
// messages that carry a ComparisonError) and the phase gate — whether the
// legacy cert-manager platform-app-ca Secret, the OpenBao ClusterSecretStore,
// and the CA chain are present/Ready.
// Best-effort throughout; group titles keep it scannable in the run log.
func diagnoseConvergence(argoNS string) {
	diagGroup("convergence — Argo CD Applications (sync / health / condition messages)", func() {
		// One line per app with its condition messages inline — a child's
		// "ComparisonError: ... app path does not exist" is visible at a glance.
		diagStream("kubectl", "-n", argoNS, "get", "applications",
			"-o", "custom-columns=NAME:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status,MESSAGE:.status.conditions[*].message")
	})
	diagGroup("convergence — platform-bootstrap seed Application (full status)", func() {
		// The seed app-of-apps; its conditions + operationState.message explain an
		// OutOfSync/Missing stall (and whether a child app poisoned its sync).
		diagStream("kubectl", "-n", argoNS, "get", "application", "platform-bootstrap", "-o", "yaml")
	})
	diagGroup("convergence — phase gate: platform-app-ca, OpenBao store, CA chain", func() {
		// platform-app-ca is legacy but still useful context; OpenBao store Ready is
		// the post-bootstrap signal that ends phase1. Capture both plus the CA chain.
		diagStream("kubectl", "-n", "cert-manager", "get", "secret", "platform-app-ca", "-o", "wide")
		diagStream("kubectl", "get", "clustersecretstore", "openbao", "-o", "wide")
		diagStream("kubectl", "get", "certificate,certificaterequest", "--all-namespaces", "-o", "wide")
		diagStream("kubectl", "get", "clusterissuer", "-o", "wide")
	})
}

// diagnoseFailingWorkloads dumps describe + previous/current container logs for
// every crashlooping / not-starting pod and every failed Job across ALL
// namespaces. The namespace sweeps and Argo/CA captures above report STATES; a
// workload's root cause is a container LOG they never grab — so on a torn-down
// cluster this is the only record of WHY a workload failed (e.g. otomi-api
// CrashLoopBackOff, harbor-robot-provisioner Job Failed). Best-effort throughout:
// PodIsFailing is the same predicate the convergence gate uses, so this captures
// exactly the pods that pinned it.
func diagnoseFailingWorkloads() {
	diagGroup("convergence — failing-workload logs (crashloop / not-starting pods + failed Jobs)", func() {
		for _, raw := range kItems("get", "pods", "-A") {
			var p struct {
				Metadata struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
				} `json:"metadata"`
				Status health.PodStatus `json:"status"`
			}
			if json.Unmarshal(raw, &p) != nil || !health.PodIsFailing(p.Status) {
				continue
			}
			fmt.Printf("### %s/%s — %s\n", p.Metadata.Namespace, p.Metadata.Name, health.SummarizeStates(p.Status))
			diagStream("kubectl", "-n", p.Metadata.Namespace, "describe", "pod", p.Metadata.Name)
			all := append(append([]health.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
			for _, c := range all {
				diagStream("kubectl", "-n", p.Metadata.Namespace, "logs", p.Metadata.Name, "-c", c.Name, "--previous", "--tail=60")
				diagStream("kubectl", "-n", p.Metadata.Namespace, "logs", p.Metadata.Name, "-c", c.Name, "--tail=40")
			}
		}
		for _, raw := range kItems("get", "jobs", "-A") {
			var j struct {
				Metadata struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
				} `json:"metadata"`
				Status struct {
					Failed int `json:"failed"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &j) != nil || j.Status.Failed == 0 {
				continue
			}
			fmt.Printf("### job %s/%s (failed=%d)\n", j.Metadata.Namespace, j.Metadata.Name, j.Status.Failed)
			diagStream("kubectl", "-n", j.Metadata.Namespace, "logs", "job/"+j.Metadata.Name, "--all-containers", "--tail=120")
		}
	})
}

// diagGroup wraps fn in a collapsible ::group::/::endgroup:: block for the run
// log. Package-level so the per-namespace sweep can share it.
func diagGroup(title string, fn func()) {
	fmt.Printf("::group::%s\n", title)
	fn()
	fmt.Println("::endgroup::")
}

// diagnoseNamespace dumps the install-failure picture for one namespace: its
// resources, Jobs (+ logs), per-pod describes, recent events, and the Helm
// status/history for release. Every probe is best-effort; group titles carry
// the namespace so the two sweeps stay distinguishable in the run log.
func diagnoseNamespace(ns, release string) {
	diagGroup(ns+" — all resources", func() {
		diagStream("kubectl", "get", "all", "-n", ns, "-o", "wide")
	})
	diagGroup(ns+" — Jobs", func() {
		diagStream("kubectl", "get", "jobs", "-n", ns, "-o", "wide")
	})
	diagGroup(ns+" — Pods (wide): Pending / ImagePullBackOff / Error", func() {
		diagStream("kubectl", "get", "pods", "-n", ns, "-o", "wide")
	})
	diagGroup(ns+" — describe every pod (scheduling + pull errors)", func() {
		for _, p := range kubectlNames("-n", ns, "get", "pods", "-o", "name") {
			fmt.Printf("----- describe %s -----\n", p)
			diagStream("kubectl", "describe", "-n", ns, p)
		}
	})
	diagGroup(ns+" — logs from Job pods", func() {
		for _, j := range kubectlNames("-n", ns, "get", "jobs", "-o", "name") {
			fmt.Printf("----- logs %s -----\n", j)
			diagStream("kubectl", "logs", "-n", ns, j, "--all-containers", "--tail=200")
		}
	})
	diagGroup(ns+" — recent events (by time)", func() {
		if out, err := execOutput("kubectl", "get", "events", "-n", ns, "--sort-by=.lastTimestamp"); err == nil {
			fmt.Print(tailLines(string(out), 60))
			fmt.Println()
		}
	})
	diagGroup(ns+" — Helm release "+release+" status / history", func() {
		diagStream("helm", "status", release, "-n", ns)
		diagStream("helm", "history", release, "-n", ns)
	})
}

// kubectlNames returns the non-empty lines of a `kubectl ... -o name` listing,
// nil on any error (the diagnostics' best-effort contract).
func kubectlNames(args ...string) []string {
	out, err := execOutput("kubectl", args...)
	if err != nil {
		return nil
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			names = append(names, l)
		}
	}
	return names
}
