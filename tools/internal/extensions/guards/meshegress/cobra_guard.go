package meshegress

// cobra_guard.go — the CLI surface for guard.
//
// Split from guard.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func Cmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "mesh-egress-guard",
		Short: "fail when a NetworkPolicy egresses to a STRICT-mesh namespace from outside that mesh",
		Long: "Static guard for the harbor-reconciler mesh-isolation class: a pod outside an\n" +
			"Istio STRICT-mTLS namespace (e.g. harbor) cannot reach a Service inside it, so a\n" +
			"NetworkPolicy that egresses there from a different namespace describes traffic that\n" +
			"will be silently dropped at the sidecar. Run the client IN that namespace instead.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return Run(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return cmd
}
