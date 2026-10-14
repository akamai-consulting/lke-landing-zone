package callerperms

// cobra_guard.go — the CLI surface for Run. Flag wiring and help text only.

import "github.com/spf13/cobra"

// Cmd is `llz ci reusable-workflow-caller-permissions`.
func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "reusable-workflow-caller-permissions",
		Short: "fail when a job calling a local reusable workflow grants less than its jobs need",
		Long: "A called workflow's jobs can never hold more than the CALLING job's token. Ask\n" +
			"for more and GitHub refuses to start the RUN — `startup_failure`, with no jobs,\n" +
			"no step logs and no annotations, so nothing inside the run explains it.\n\n" +
			"That is not hypothetical: the delivered secret-rotation.yml granted its call job\n" +
			"only `contents: read` while the reusable body's propagate-linode-pat job asks for\n" +
			"`id-token: write`, so every scheduled run died at startup and the credential\n" +
			"rotation never once executed — while looking configured.\n\n" +
			"Checks local calls (`uses: ./.github/workflows/x.yml`) in this repo's workflows\n" +
			"and in instance-template's, resolving the callee beside its caller. Remote\n" +
			"`org/repo/...@ref` calls are out of scope: they cannot be read from this tree.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Run(root, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	return c
}
