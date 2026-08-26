package upgradeplan

// cobra_guard.go — the CLI surface for Run.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
)

// Cmd is `llz ci assert-upgrade-plan`.
func Cmd() *cobra.Command {
	var plan string
	var reportOnly bool
	var expectNoChanges bool
	c := &cobra.Command{
		Use:   "assert-upgrade-plan",
		Short: "fail when a plan taken after an upgrade would destroy or replace a live resource",
		Long: "Reads `tofu show -json` output and fails if any resource would be deleted or\n" +
			"replaced.\n\n" +
			"WHAT IT IS FOR. Every e2e lane force-pushes a fresh instantiation at the commit\n" +
			"under test, so the only configuration the release gate exercises is greenfield.\n" +
			"No lane plans a new template against state an OLDER release created — which is\n" +
			"where an adopter lives from their second day on, and where a module change that\n" +
			"reads as a small correct diff can propose recycling a live cluster.\n\n" +
			"Creates and in-place updates pass; an upgrade legitimately adds resources and\n" +
			"changes attributes. A delete anywhere in a resource's planned actions is a\n" +
			"finding, which covers a bare destroy and both spellings of a replace.\n\n" +
			"--expect-no-changes tightens the question from \"proposes no destruction\" to\n" +
			"\"proposes NOTHING\". That is the right question immediately after an apply, when\n" +
			"the state was just made to match the configuration and any remaining diff is a\n" +
			"resource Terraform cannot bring to rest — a perpetual diff that will churn every\n" +
			"future apply.\n\n" +
			"It cannot see a destructive change Terraform models as an in-place update —\n" +
			"linode_lke_cluster's create-time-only vpc_id is exactly that, and its gate is the\n" +
			"coupling test in tfroots. --expect-no-changes DOES catch it, from the other side:\n" +
			"an attribute the API ignores on update re-proposes itself forever, so a plan taken\n" +
			"after an apply is not empty.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := Run(plan, expectNoChanges, cmd.OutOrStdout(), cmd.ErrOrStderr(), cmd.InOrStdin())
			if err != nil && reportOnly {
				// REPORTED, NOT SWALLOWED. The finding is printed either way; this
				// only decides whether it stops the run.
				fmt.Fprintf(cmd.ErrOrStderr(), "\n(--report-only: %v — this is a PLAN, so nothing was "+
					"changed and nothing is blocked. The same check FAILS on the apply.)\n", err)
				return nil
			}
			return err
		},
	}
	c.Flags().StringVar(&plan, "plan", "-", "`tofu show -json` output to read (\"-\" for stdin)")
	// DEFAULTED FROM THE ENVIRONMENT so the workflow can select it WITHOUT
	// interpolating `${{ }}` into a run: script — which is what workflow-injection
	// forbids, and rightly: env.ACTION descends from a dispatch input. `env:` is the
	// documented mitigation, and it keeps the step a single fixed command line.
	c.Flags().BoolVar(&reportOnly, "report-only", cli.EnvBool("LLZ_ASSERT_PLAN_REPORT_ONLY", false),
		"print the verdict but exit 0 — for the PLAN lane, where the finding is a preview rather than a refusal (env: LLZ_ASSERT_PLAN_REPORT_ONLY)")
	c.Flags().BoolVar(&expectNoChanges, "expect-no-changes", false,
		"also fail on any NON-destructive change — for a plan taken straight after an apply, where an empty plan is the only correct answer")
	return c
}
