package main

// ci_clear_secrets.go implements `llz ci clear-cluster-secrets` — the native port
// of cluster-bootstrap's null_resource.clear_openbao_secrets_on_destroy. After a
// cluster teardown the GH environment secrets bound to the previous OpenBao /
// Harbor instances are not just stale but actively harmful on the next bootstrap
// (e.g. a revoked OPENBAO_ROOT_TOKEN loaded by the next Configure step), so the
// teardown deletes them; each fresh apply re-seeds whatever is needed.
//
// Relocated from a TF destroy-time provisioner to an explicit CI destroy step so
// the logic is unit-tested Go rather than an inline `gh secret delete` heredoc,
// and so removing the resource can't fire a destructive provisioner on a later
// apply (the resource is dropped via a `removed` block). Best-effort: a missing
// secret (HTTP 404) or a per-secret API hiccup is a warning, never a teardown
// failure — the cluster is going away regardless.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// clusterScopedSecrets are the GH environment secrets bound to THIS cluster's
// OpenBao / Harbor — safe to delete on teardown. Anything not listed is
// operator-shared (LINODE_API_TOKEN, APL_VALUES_REPO_TOKEN, TF_STATE_*,
// OPENBAO_SECRETS_WRITE_TOKEN itself, …) and must never be deleted. Both AppRole
// secret-id names are listed: the active peer owns OPENBAO_APPROLE_SECRET_ID and
// the standby OPENBAO_APPROLE_SECRET_ID_STANDBY — deleting the one this cluster
// doesn't own is a harmless 404, so no ha_role lookup is needed.
var clusterScopedSecrets = []string{
	"OPENBAO_ROOT_TOKEN",
	"OPENBAO_RECOVERY_KEY_1",
	"OPENBAO_RECOVERY_KEY_2",
	"OPENBAO_RECOVERY_KEY_3",
	"OPENBAO_APPROLE_SECRET_ID",
	"OPENBAO_APPROLE_SECRET_ID_STANDBY",
	"HARBOR_ROBOT_NAME",
	"HARBOR_PASSWORD",
}

// ghDeleteSecretFn deletes one env-scoped GitHub secret. gh resolves auth + repo
// from the ambient GH_TOKEN/GH_REPO. Seamed for tests.
var ghDeleteSecretFn = func(name, ghEnv string) error {
	cmd := exec.Command("gh", "secret", "delete", name, "--env", ghEnv)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh secret delete %s --env %s: %s", name, ghEnv, strings.TrimSpace(string(out)))
	}
	return nil
}

func ciClearClusterSecretsCmd() *cobra.Command {
	var ghEnv string
	c := &cobra.Command{
		Use:   "clear-cluster-secrets",
		Short: "delete the cluster-scoped OpenBao/Harbor GitHub environment secrets on teardown",
		Long: "Native port of null_resource.clear_openbao_secrets_on_destroy. Deletes the\n" +
			"GH environment secrets bound to THIS cluster's OpenBao + Harbor (root token,\n" +
			"recovery keys, both AppRole secret-id names, Harbor robot creds) from the --env\n" +
			"Environment, so a destroyed cluster's stale secrets can't poison the next\n" +
			"bootstrap. Best-effort: a 404 (already absent) or a per-secret API failure is\n" +
			"a warning, not an error. Auth: the ambient GH_TOKEN (a secrets:write PAT); an\n" +
			"unset GH_TOKEN is a logged no-op (the operator clears them manually).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIClearClusterSecrets(ghEnv) },
	}
	c.Flags().StringVar(&ghEnv, "env", "", "GitHub Environment to clear, e.g. infra-primary (required)")
	return c
}

func runCIClearClusterSecrets(ghEnv string) error {
	if ghEnv == "" {
		return fmt.Errorf("--env is required, e.g. infra-primary")
	}
	if os.Getenv("GH_TOKEN") == "" {
		fmt.Printf("::warning::GH_TOKEN not set — skipping GH env-secret cleanup. Manually clear from %s: %s\n",
			ghEnv, strings.Join(clusterScopedSecrets, " "))
		return nil
	}
	for _, s := range clusterScopedSecrets {
		// 404 (secret absent) and any other per-secret failure are non-fatal: the
		// cluster IS being destroyed, so blocking teardown on a secrets-API hiccup
		// is disproportionate — surface a warning and continue.
		if err := ghDeleteSecretFn(s, ghEnv); err != nil {
			fmt.Printf("::warning::Could not delete %s / %s (already absent, or token lacks scope): %v\n", ghEnv, s, err)
			continue
		}
		fmt.Printf("Deleted GH env secret %s / %s\n", ghEnv, s)
	}
	fmt.Printf("OpenBao/Harbor GH env-secret cleanup complete for %s.\n", ghEnv)
	return nil
}
