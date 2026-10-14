package assertidentity

// cobra_openbaoext.go — the CLI surface for the spec.openbao kubernetes-role lane.

import "github.com/spf13/cobra"

func AssertOpenBaoExtensionsCmd() *cobra.Command {
	var requireBound bool
	c := &cobra.Command{
		Use:   "assert-openbao-extensions",
		Short: "verify every spec.openbao kubernetes role against what OpenBao enforces",
		Long: "For every spec.openbao.kubernetesRoles entry: mints a TokenRequest for the\n" +
			"bound service account, logs in through THAT role, and requires the token to\n" +
			"carry exactly the declared policies, every declared capability\n" +
			"(sys/capabilities-self), and each mount its rules reach with the declared\n" +
			"engine type. Read-only on OpenBao; the only write is the short-lived\n" +
			"TokenRequest. A bound service account that does not exist yet is a warning\n" +
			"(the workload is not deployed) unless --require-bound. No spec.openbao roles\n" +
			"is a clean no-op. The operator grants are checked by team-login-smoke.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runAssertOpenBaoExtensions(requireBound) },
	}
	c.Flags().BoolVar(&requireBound, "require-bound", false, "fail, rather than warn, when a bound service account does not exist (the e2e lane, whose fixture binds the ESO controller)")
	return c
}
