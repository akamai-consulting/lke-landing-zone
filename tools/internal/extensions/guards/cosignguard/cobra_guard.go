package cosignguard

// cobra_guard.go — the CLI surface for guard.
//
// Split from guard.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "cosign-subject-guard",
		Short: "fail when a cosign keyless subject names a workflow that no longer exists",
		Long: "Scans the platform manifest trees for Kyverno keyless `subject:` pins that\n" +
			"identify a GitHub Actions workflow, and fails if the named workflow file is\n" +
			"missing from .github/workflows/. Keyless signing derives the certificate\n" +
			"subject from the workflow's path, so renaming the signing workflow silently\n" +
			"invalidates every signature the policy will accept — surfacing not here, but\n" +
			"as pods that fail admission in every downstream cluster.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return Run(root)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root")
	return c
}
