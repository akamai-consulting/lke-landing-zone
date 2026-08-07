package argodiag

// cobra_argodiag.go — the `llz ci diagnose-argocd` flag set.
//
// This package declares the extension; what follows is its CLI surface — flag
// wiring and help text, and nothing that decides anything.
//
// NO Deps STRUCT, and that is the extraction's cheapest finding. The measured
// closure was three symbols, two of them noise; the one real seam was
// `execOutput`, and internal/kubectlprobe already exports `Exec` with the
// identical signature — a package the diagnostic ALREADY imported for Reachable()
// and Items(). Clause three of the Deps rule ("is it already injectable
// elsewhere?") answered it, so nothing needed injecting. `diagStream` travelled
// with the code: it was already a package var precisely so tests could record the
// probe sequence.

import (
	"github.com/spf13/cobra"
)

func DiagnoseArgoCDCmd() *cobra.Command {
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
			"Skips cleanly when $KUBECONFIG is absent/empty (cluster may not exist) or\n" +
			"when the apiserver is unreachable (e.g. the runner was never allowlisted on\n" +
			"the control-plane firewall) — otherwise every probe would block on its own\n" +
			"~30s dial timeout and the dozens of them would burn the whole job budget.\n" +
			"Always exits 0: diagnostics must never mask the failure that triggered them.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return Run(aplNS, ns) },
	}
	c.Flags().StringVar(&ns, "namespace", "argocd", "namespace holding the ArgoCD install")
	c.Flags().StringVar(&aplNS, "apl-namespace", "apl-operator", "namespace holding the apl-operator install")
	return c
}
