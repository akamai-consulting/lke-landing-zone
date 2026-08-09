package assertsecrets

// cobra_broadpat.go — the CLI surface for broadpat.
//
// Split from broadpat.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func BroadPATRotationCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "assert-broad-pat-rotation",
		Short: "e2e: force one broad-PAT rotation Job and assert it rotated (mint→OpenBao→GitHub→revoke)",
		Long: "e2e gate that EXERCISES the in-cluster broad-PAT rotator: creates a one-off\n" +
			"Job from the broad-pat-rotator CronJob (--apply, seeded rotated_at=0 makes it\n" +
			"due), waits for it, and asserts the audit record reports action=rotated. No-ops\n" +
			"unless spec.components.broadPatRotator is enabled for --region. Safe because e2e\n" +
			"enables the rotator with an e2e-unique label + broadPATDeployments=e2e, so the\n" +
			"mint/revoke touch only the e2e PAT family and only infra-e2e is rewritten. Uses\n" +
			"the job's default kubeconfig, same as the converge poll it runs after.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runAssertBroadPATRotation(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) whose broadPatRotator toggle gates the exercise (required)")
	return c
}
