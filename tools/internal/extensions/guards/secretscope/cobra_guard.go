package secretscope

// cobra_guard.go — the CLI surface for Run. Flag wiring and help text only.

import "github.com/spf13/cobra"

// Cmd is `llz ci workflow-secret-scope`.
func Cmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "workflow-secret-scope",
		Short: "fail when a job reads an environment-scoped secret it cannot resolve",
		Long: "GitHub resolves `secrets.X` against the repo scope plus the job's `environment:`,\n" +
			"if it declares one. An environment-scoped secret read from a job without one is\n" +
			"not an error — it is the EMPTY STRING, and nothing in the log says a secret was\n" +
			"asked for and not found. What the operator sees is whatever the tool does with an\n" +
			"empty credential, several layers from the cause.\n\n" +
			"llz-terraform.yml's `plan-cluster-pr` shipped exactly that: it read\n" +
			"TF_STATE_ACCESS_KEY, TF_STATE_SECRET_KEY and LINODE_API_TOKEN — all three marked\n" +
			"EnvScope in the requirement table — from a job with no `environment:`, so every\n" +
			"plan on every adopter's PR died at `tofu init`.\n\n" +
			"The second arm is the repair that looks obvious and is not. `llz` locks every\n" +
			"infra-<env> environment to ref=main, which is the boundary that stops a pushed\n" +
			"branch from having the OpenBao unseal keys injected into a job it controls. A\n" +
			"pull_request run's ref is refs/pull/N/merge, so adding the environment moves the\n" +
			"failure from `tofu init` to job start. A pull-request job cannot hold these\n" +
			"credentials at all, and this says so instead of letting the next author\n" +
			"rediscover it one wrong fix at a time.\n\n" +
			"Scans this repo's workflows and the DELIVERED copies under instance-template/.\n" +
			"Reachability follows local `uses: ./.github/workflows/…` calls through the\n" +
			"calling job's own event gate, so a dispatch-only caller does not put its callee\n" +
			"on the pull-request path. Which secrets are environment-scoped comes from\n" +
			"envreq.E2ERequirements — the table `llz doctor` and `require-repo-config` read —\n" +
			"never from the workflows under test.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Run(root, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repo root to scan")
	return c
}
