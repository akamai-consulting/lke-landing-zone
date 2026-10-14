package k8sminorcoherence

// cobra_guard.go — the CLI surface for Run.
//
// Split from guard.go so the directory shows its commands at a glance: every file
// named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"os"

	"github.com/spf13/cobra"
)

// Cmd is `llz ci k8s-minor-coherence`.
func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "k8s-minor-coherence",
		Short: "the kind cluster lint.yml dry-runs against must be the Kubernetes minor we deploy",
		Long: "lint.yml's `kubectl apply --dry-run=server` is the only gate that asks a real\n" +
			"API server whether the rendered manifests are acceptable. It is answered by the\n" +
			"kind cluster that job creates — so the minor that cluster runs decides what the\n" +
			"gate can see.\n\n" +
			"It pinned kind's VERSION and not its NODE IMAGE, so the answer came from kind's\n" +
			"own default (v1.31.2) while the cluster Terraform root pinned v1.34.6+lke2.\n" +
			"Three minors of API churn sat outside the gate's field of view and it stayed\n" +
			"green, because accepting those manifests is an ordinary thing for a 1.31 server\n" +
			"to do. Neither pin was wrong on its own; only the relation was.\n\n" +
			"Compares the MINOR, which is the precision the API surface moves at — the two\n" +
			"sides can never be equal (Linode ships v1.34.6+lke2, kind ships v1.34.8). Also\n" +
			"holds the job's kubectl to its supported ±1 skew from that server, and refuses a\n" +
			"kind step that pins neither its node image nor its kubectl, since both then fall\n" +
			"back to the action's defaults.\n\n" +
			"Runs offline — no cluster, no network.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return Run(root, os.Stdout, os.Stderr)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root to scan")
	return c
}
