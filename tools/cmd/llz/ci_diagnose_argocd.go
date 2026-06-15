package main

// ci_diagnose_argocd.go implements `llz ci diagnose-argocd` — the native port
// of llz-terraform.yml's 'Diagnose ArgoCD install failure' step. Runs only on
// the failure path (typically helm_release.argocd timing out on a pre-install
// hook) and dumps everything needed to see WHY the hook Job / ArgoCD pods are
// not becoming ready. Diagnostics must never mask the original failure, so
// every probe is best-effort and the command always exits 0.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

func ciDiagnoseArgoCDCmd() *cobra.Command {
	var ns string
	c := &cobra.Command{
		Use:   "diagnose-argocd",
		Short: "dump ArgoCD install-failure diagnostics (best-effort, never fails)",
		Long: "Native port of the 'Diagnose ArgoCD install failure' step. Dumps node\n" +
			"schedulability, the argocd namespace's resources, the Helm pre-install hook\n" +
			"Jobs + their logs, per-pod describes, recent events, and the Helm release\n" +
			"history — grouped with ::group:: for the run log. Skips cleanly when\n" +
			"$KUBECONFIG is absent/empty (cluster may not exist). Always exits 0:\n" +
			"diagnostics must never mask the failure that triggered them.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIDiagnoseArgoCD(ns) },
	}
	c.Flags().StringVar(&ns, "namespace", "argocd", "namespace holding the ArgoCD install")
	return c
}

// diagStream runs a command with output streamed to stdout, best-effort. A
// package var so tests can record the probe sequence without real binaries.
var diagStream = func(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	_ = cmd.Run()
}

func runCIDiagnoseArgoCD(ns string) error {
	kc := os.Getenv("KUBECONFIG")
	if st, err := os.Stat(kc); kc == "" || err != nil || st.Size() == 0 {
		fmt.Fprintln(os.Stderr, "::warning::No kubeconfig available — cluster may not exist; nothing to diagnose")
		return nil
	}

	group := func(title string, fn func()) {
		fmt.Printf("::group::%s\n", title)
		fn()
		fmt.Println("::endgroup::")
	}

	group("Nodes (schedulable? Ready?)", func() {
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
	group(ns+" namespace — all resources", func() {
		diagStream("kubectl", "get", "all", "-n", ns, "-o", "wide")
	})
	group("Helm pre-install hook Jobs", func() {
		diagStream("kubectl", "get", "jobs", "-n", ns, "-o", "wide")
	})
	group("Pods (wide) — look for Pending / ImagePullBackOff / Error", func() {
		diagStream("kubectl", "get", "pods", "-n", ns, "-o", "wide")
	})
	group("Describe every pod in "+ns+" (scheduling + pull errors)", func() {
		for _, p := range kubectlNames("-n", ns, "get", "pods", "-o", "name") {
			fmt.Printf("----- describe %s -----\n", p)
			diagStream("kubectl", "describe", "-n", ns, p)
		}
	})
	group("Logs from hook Job pods", func() {
		for _, j := range kubectlNames("-n", ns, "get", "jobs", "-o", "name") {
			fmt.Printf("----- logs %s -----\n", j)
			diagStream("kubectl", "logs", "-n", ns, j, "--all-containers", "--tail=200")
		}
	})
	group("Recent events ("+ns+", by time)", func() {
		if out, err := execOutput("kubectl", "get", "events", "-n", ns, "--sort-by=.lastTimestamp"); err == nil {
			fmt.Print(tailLines(string(out), 60))
			fmt.Println()
		}
	})
	group("Helm release status / history", func() {
		diagStream("helm", "status", "argocd", "-n", ns)
		diagStream("helm", "history", "argocd", "-n", ns)
	})

	fmt.Println("Diagnostics complete. Common causes for 'failed pre-install: timed out':")
	fmt.Println("  • hook pod stuck Pending  -> nodes not Ready/schedulable yet (check Taints / Conditions above)")
	fmt.Println("  • ImagePullBackOff        -> registry unreachable or image pull secret missing")
	fmt.Println("  • CrashLoopBackOff        -> see hook Job logs above")
	return nil
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
