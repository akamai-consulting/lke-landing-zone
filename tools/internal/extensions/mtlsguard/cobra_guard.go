package mtlsguard

// cobra_guard.go — the CLI surface for guard.
//
// Split from guard.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func Cmd() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "mtls-wiring-guard",
		Short: "fail when an OpenBao-consuming workload does not mount the mTLS material its code reads",
		Long: "Gate on the correspondence between a workload's code path and its pod spec\n" +
			"(docs/adr/0010-in-cluster-mtls.md). Any pod declaring OPENBAO_ADDR calls\n" +
			"openbao.InClusterHTTPClient(), which reads a CA bundle and a client keypair — so\n" +
			"the pod must mount paths covering all three, every TLS Secret it mounts must\n" +
			"be created by a Certificate in the same namespace, and OPENBAO_SKIP_VERIFY\n" +
			"must not reappear.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return Run(root) },
	}
	cmd.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return cmd
}
