package main

// ci_tokeninv.go — the cobra surface for the `token-inventory` extension
// (internal/tokeninv). The probes, the expiry policy, the capability scars and
// the rotation decision table live in the package; what stays here is flag
// parsing and the Deps wiring.

import (
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tokeninv"
	"github.com/spf13/cobra"
)

// tokenInvDepsFor hands the extension the capabilities it declares. Both fields
// are real implementations — a fixture that no-ops Summary would make the
// rotation plan's assertions vacuous, since its entire output IS the summary
// plus the job-gating outputs.
func tokenInvDepsFor() tokeninv.Deps {
	return tokeninv.Deps{
		CloudToken: linode.TokenFromEnv,
		Summary:    ghaout.Append,
	}
}

func ciTokenInventoryCmd() *cobra.Command {
	var namespace, name string
	var maxDays, warnDays int
	c := &cobra.Command{
		Use:   "token-inventory",
		Short: "measure CI-token expiry and emit the ConfigMap the reconciler re-exposes as metrics",
		Long: "Writer half of the credential single-pane-of-glass. Measures the expiry of the\n" +
			"external CI tokens this job holds — the GitHub service PATs in ghPATTargets\n" +
			"(OPENBAO_SECRETS_WRITE_TOKEN, APL_VALUES_REPO_TOKEN, and E2E_DISPATCH_TOKEN /\n" +
			"GHCR_READ_TOKEN when set) via the token-expiration header, and every Linode PAT\n" +
			"via the Linode API — and emits a ConfigMap (metadata only, never a token value) to\n" +
			"stdout. Pipe it to `kubectl apply -f -`; the in-cluster llz-reconciler re-exposes it\n" +
			"as llz_token_expiry_timestamp_seconds so Prometheus alerts before expiry.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := tokeninv.RunInventory(cmd.Context(), tokenInvDepsFor(), namespace, name, maxDays, warnDays)
			if err != nil {
				return err
			}
			fmt.Println(out)
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&namespace, "namespace", "llz-reconciler", "namespace of the inventory ConfigMap the reconciler reads")
	f.StringVar(&name, "name", "llz-token-inventory", "name of the inventory ConfigMap")
	f.IntVar(&maxDays, "max-days", 90, "flag a token whose lifetime exceeds this many days as a breach")
	f.IntVar(&warnDays, "warn-days", 14, "mark a token expiring within this many days as warn")
	return c
}

func ciValidateTokensCmd() *cobra.Command {
	var failOnInvalid bool
	c := &cobra.Command{
		Use:   "validate-tokens",
		Short: "probe every pipeline credential for validity AND capability, before anything provisions",
		Long: "CI counterpart of the local `llz doctor` validity probe. Reads each pipeline\n" +
			"credential from the ENVIRONMENT (where CI injects the repo/infra-<env> secrets)\n" +
			"and actively probes it, so a set-but-dead token fails FAST with 'rotate it'\n" +
			"instead of 401/403-ing deep inside a 45-minute provision. Probes two independent\n" +
			"things per credential: validity (does it authenticate?) and capability (is it\n" +
			"scoped for the job it exists for?) — an under-scoped PAT authenticates perfectly\n" +
			"and still 403s on the operation.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return tokeninv.RunValidate(failOnInvalid) },
	}
	c.Flags().BoolVar(&failOnInvalid, "fail-on-invalid", true, "exit non-zero when any required credential is invalid or under-scoped")
	return c
}

func ciRotationPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotation-plan",
		Short: "route a rotation run: schedule/scope → job-gating step outputs",
		Long: "Native port of llz-secret-rotation.yml's 'Route scope + validate emergency\n" +
			"confirmation' step. Maps the trigger (schedule cron, or a dispatch scope +\n" +
			"typed confirmation + reason) onto the run-*/apply step outputs the rotation\n" +
			"jobs gate on, and writes the dispatch audit summary. Fails on a confirm\n" +
			"mismatch, a blank reason, or an unknown scope/cron — nothing downstream\n" +
			"runs unless this routing passed. Env: EVENT, CRON, SCOPE, REGION, CONFIRM,\n" +
			"REASON, *_APPLY, ACTOR, DEPLOYMENTS.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return tokeninv.RunRotationPlan(tokenInvDepsFor(), tokeninv.InputsFromEnv())
		},
	}
}
