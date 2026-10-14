package defaultdeny

// cobra_guard.go — the CLI surface for Run. Flag wiring and help text only.

import "github.com/spf13/cobra"

// Cmd is `llz ci default-deny-egress`.
func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "default-deny-egress",
		Short: "fail when a pod whose egress is policed is granted no egress at all",
		Long: "NetworkPolicies are additive and there is no deny rule. A namespace-wide policy\n" +
			"with `podSelector: {}` and `policyTypes: [Egress]` starts policing EVERY pod in the\n" +
			"namespace, and any pod no companion policy selects is left unable to reach anything\n" +
			"— not the apiserver, not DNS. Nothing reports it: the pod stays 1/1 Running with\n" +
			"healthy endpoints and whatever it was deployed to do never happens.\n\n" +
			"openbao-cert-watcher shipped in that state. llz-openbao-platform's\n" +
			"`openbao-default-deny` polices the whole llz-openbao namespace and its one companion\n" +
			"selects `app.kubernetes.io/name: openbao`; the watcher carries\n" +
			"`openbao-cert-watcher` and its own policy declared Ingress only. Every kubectl call\n" +
			"it made was dropped, and at certificate renewal nothing restarted OpenBao.\n\n" +
			"Reads the platform tree AND the rendered charts, and matches them by namespace —\n" +
			"the default-deny is usually in a chart and the pod it strands is usually in\n" +
			"platform-apl/, so neither tree read alone shows anything wrong.\n\n" +
			"It does NOT judge whether the egress a pod is granted is sufficient; that is not\n" +
			"decidable here. The line it draws is policed-and-granted-nothing.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return Run(root) },
	}
	c.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return c
}
