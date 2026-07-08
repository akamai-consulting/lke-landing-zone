package main

// ci_openbao_login.go — `llz ci openbao-login`: exchange a GitHub Actions OIDC
// token for a short-lived OpenBao token via the `jwt` auth method, over a direct
// HTTPS call to OpenBao's API, and export it to $GITHUB_ENV for later steps.
//
// This is the auth primitive behind the SECRETLESS day-2 thin-caller pattern
// (docs/designs/cross-org-reuse-pattern.md). A day-2 workflow that runs on an
// IN-CLUSTER runner (one that can reach OpenBao's ClusterIP) needs no GitHub
// secrets and no `secrets: inherit`: it declares `permissions: id-token: write`,
// runs this command, and downstream `llz` steps use $OPENBAO_TOKEN. GitHub OIDC
// tokens are minted per-job and are NOT subject to the cross-org
// `secrets: inherit` limitation (#200), so this works identically for an adopter
// in a different org from the template — the reuse boundary disappears.
//
// It deliberately does NOT go through `kubectl exec` into the OpenBao pod (the
// hosted-runner path `llz ci rotate-incluster-pat` uses); a direct API login is
// what an in-cluster runner wants, and it keeps this usable from any pod that can
// reach OpenBao. The role's bound_claims pin it to this instance repo
// (`llz ci bao-configure`), so a leaked OIDC token from another repo can't use it.

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
	"github.com/spf13/cobra"
)

// inClusterOpenBaoAddr is the ClusterIP address an in-cluster runner reaches
// OpenBao at — the same endpoint the reconciler and CronJobs use.
const inClusterOpenBaoAddr = "https://platform-openbao.llz-openbao.svc.cluster.local:8200"

func ciOpenBaoLoginCmd() *cobra.Command {
	var role, addr, exportVar string
	c := &cobra.Command{
		Use:   "openbao-login",
		Short: "exchange a GitHub OIDC token for an OpenBao token (jwt auth) and export it",
		Long: "Mints a GitHub Actions OIDC token (needs `permissions: id-token: write`) and\n" +
			"exchanges it for a short-lived OpenBao token via the `jwt` auth method, then\n" +
			"writes it to $GITHUB_ENV as OPENBAO_TOKEN (override with --export-var) and\n" +
			"masks it. The auth primitive for secretless in-cluster day-2 workflows — no\n" +
			"GitHub secrets, no `secrets: inherit`, works cross-org (see\n" +
			"docs/designs/cross-org-reuse-pattern.md). Run on a runner that can reach\n" +
			"OpenBao's API (in-cluster).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOpenBaoLogin(gopts, role, addr, exportVar)
		},
	}
	c.Flags().StringVar(&role, "role", "platform-ci", "OpenBao jwt role to log in as (bao-configure defines platform-ci, secret-propagator)")
	c.Flags().StringVar(&addr, "addr", "", "OpenBao API address (default: $OPENBAO_ADDR, else the in-cluster ClusterIP)")
	c.Flags().StringVar(&exportVar, "export-var", "OPENBAO_TOKEN", "$GITHUB_ENV variable to export the token as")
	return c
}

func runOpenBaoLogin(g globalOpts, role, addr, exportVar string) error {
	if addr == "" {
		if addr = os.Getenv("OPENBAO_ADDR"); addr == "" {
			addr = inClusterOpenBaoAddr
		}
	}
	ghRepo := os.Getenv("GITHUB_REPOSITORY")
	if ghRepo == "" {
		return fmt.Errorf("GITHUB_REPOSITORY is empty — cannot derive the OIDC audience for the %s jwt login", role)
	}
	if g.dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) openbao-login role=%s addr=%s export=%s\n", role, addr, exportVar)
		return nil
	}

	oidcToken, err := githubActionsOIDCToken(oidcAudienceForRepo(ghRepo), nil)
	if err != nil {
		return fmt.Errorf("mint GitHub OIDC token: %w (does the job set `permissions: id-token: write`?)", err)
	}
	maskGHALines(oidcToken)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token, err := openbao.JWTLogin(ctx, openbao.HTTPClientInsecure(30*time.Second), addr, role, oidcToken)
	if err != nil {
		return err
	}
	maskGHALines(token)

	if err := appendGHAFile("GITHUB_ENV", exportVar+"="+token); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "openbao-login: role=%s → %s exported to $GITHUB_ENV (masked)\n", role, exportVar)
	return nil
}
