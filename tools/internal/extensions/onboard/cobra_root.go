package onboard

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/spf13/cobra"
)

// cobra_root.go — the `llz <verb>` flag set, moved out of package main.
//
// An extension owns its own command; main owns the tree. These are ROOT-level
// verbs rather than `llz ci` ones, which changes nothing about the rule.

// globalOpts reads the three process-wide flags into this package's Opts.
//
// Package main used to own this as onboardOptsOf(), described there as "a
// CONVERTER, NOT AN EXPORT — handing the whole struct over would put package
// main's flag model on the other side of a package boundary". That argument
// retired with the commands: they live here now, so there is no boundary to hand
// anything across, and reading cliopts at call time is what keeps --dry-run
// late-bound.
func globalOpts() Opts {
	return Opts{DryRun: cliopts.Global.DryRun, Open: cliopts.Global.Open, Yes: cliopts.Global.Yes}
}

func TokensCmd() *cobra.Command {
	var admin bool
	var env, cluster, bucket, repo string
	c := &cobra.Command{
		Use:   "tokens",
		Short: "provision wizard: create state bucket/key, onboard.Gather PATs, push",
		Long: "Idempotently provisions an instance's credentials: creates the Terraform-\n" +
			"state OBJ bucket + a scoped key (Linode API), generates the ArgoCD deploy\n" +
			"key, gathers GitHub PATs, computes image vars, writes .llz/*.env, and pushes.\n" +
			"Skips anything already configured. --admin also wires the template repo's\n" +
			"e2e harness and defaults to the example repo. Mutating steps need --yes.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := RunTokens(globalOpts(), admin, env, cluster, bucket, repo); err != nil {
				return err
			}
			// Recommend the rest of the flow — but only after a real run (not a
			// dry-run / no-yes plan), and only standalone (`llz up` chains the
			// next gates itself, so it would be redundant there).
			if cliopts.Global.Yes && !cliopts.Global.DryRun {
				eff := env
				if eff == "" {
					eff = "e2e" // matches onboard.RunTokens' admin default
				}
				PrintNextSteps(eff)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&admin, "admin", false, "maintainer mode: also wire the template repo e2e harness; default to the example repo")
	c.Flags().StringVar(&env, "env", "", "deployment env to push to (default: e2e)")
	c.Flags().StringVar(&cluster, "cluster", "", "Linode OBJ cluster id, e.g. us-ord-1 (skip the picker)")
	c.Flags().StringVar(&bucket, "bucket", "", "state bucket name (default: <repo>-tfstate)")
	c.Flags().StringVar(&repo, "repo", "", "instance repo <owner>/<name> (default: .copier-answers.yml, or example repo in --admin)")
	return c
}
func SecretsCmd() *cobra.Command {
	s := &cobra.Command{Use: "secrets", Short: "onboard.Gather + push instance credentials"}
	s.AddCommand(
		&cobra.Command{
			Use: "onboard.Gather", Short: "paste-everything token wizard (links + .llz/*.env)",
			Args: cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error { return Gather(globalOpts(), ".") },
		},
		&cobra.Command{
			Use: "push <env>", Short: "write gathered tokens into infra-<env> (--yes)",
			Args: cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				return PushSecrets(globalOpts(), args[0])
			},
		},
	)
	return s
}
