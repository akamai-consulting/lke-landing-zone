package main

// ci_stash_secret.go implements `llz ci stash-env-secret` — the native port of
// the "terraform output → ::add-mask:: → gh secret set --env" stash blocks in
// llz-terraform.yml (the Loki admin password and the object-storage S3 key
// pairs). Terraform generates these values once (random_password / scoped OBJ
// keys); stashing them as infra-<env> Environment secrets is what makes every
// later run idempotently pass the same value back in, and what the
// bootstrap-openbao pre-flight asserts before seeding.
//
// All mappings are attempted before failing (a partial failure mid-list used
// to leave some secrets set and others not, with no flag at the top), and the
// PAT-permission remediation text lives here once instead of being duplicated
// per step.

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func ciStashEnvSecretCmd() *cobra.Command {
	var ghEnv string
	var mappings []string
	c := &cobra.Command{
		Use:   "stash-env-secret",
		Short: "store terraform outputs as GitHub environment secrets (masked, all-or-error)",
		Long: "Native port of the secret-stash steps in llz-terraform.yml. For each\n" +
			"--from-tf-output <output>=<SECRET_NAME>, reads `terraform output -raw\n" +
			"<output>` from the current working directory, ::add-mask::es it, and writes\n" +
			"it to the --env Environment via `gh secret set` (value piped over stdin,\n" +
			"never argv). An empty output is an error — the workspace should always emit\n" +
			"it. Every mapping is attempted before failing so a partial run is visible.\n" +
			"Auth: the ambient GH_TOKEN (a fine-grained PAT with Actions + Secrets:\n" +
			"write on this repo).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIStashEnvSecret(ghEnv, mappings) },
	}
	c.Flags().StringVar(&ghEnv, "env", "", "GitHub Environment to write into, e.g. infra-primary (required)")
	c.Flags().StringArrayVar(&mappings, "from-tf-output", nil, "<terraform-output>=<SECRET_NAME> mapping (repeatable, required)")
	return c
}

func runCIStashEnvSecret(ghEnv string, mappings []string) error {
	if ghEnv == "" || len(mappings) == 0 {
		return fmt.Errorf("--env and at least one --from-tf-output <output>=<SECRET_NAME> are required")
	}
	failed := false
	for _, m := range mappings {
		output, secret, ok := strings.Cut(m, "=")
		if !ok || output == "" || secret == "" {
			return fmt.Errorf("--from-tf-output must be <terraform-output>=<SECRET_NAME>, got %q", m)
		}
		raw, err := execOutput("terraform", "output", "-raw", output)
		value := strings.TrimSpace(string(raw))
		if err != nil || value == "" {
			fmt.Fprintf(os.Stderr, "::error::terraform output %s is empty — this workspace should always emit one.\n", output)
			failed = true
			continue
		}
		maskGHA(value)
		if err := ghSetSecretFn(secret, ghEnv, value); err != nil {
			fmt.Fprintf(os.Stderr, "::error::'gh secret set %s --env %s' failed: %v\n", secret, ghEnv, err)
			failed = true
			continue
		}
		fmt.Printf("Stored %s on %s.\n", secret, ghEnv)
	}
	if failed {
		fmt.Fprintf(os.Stderr, "::error::One or more secret stashes into %s failed.\n", ghEnv)
		fmt.Fprintf(os.Stderr, "::error::Most common cause: the GH_TOKEN PAT lacks access to %s (Environment) or the repo (needs Actions + Secrets: write).\n", ghEnv)
		fmt.Fprintf(os.Stderr, "::error::Verify with: GH_TOKEN=$PAT gh api repos/%s/environments/%s/secrets/public-key — must return a key_id, not 404. (gh api user only proves the token is valid, not that it can see this repo.)\n",
			os.Getenv("GITHUB_REPOSITORY"), ghEnv)
		return fmt.Errorf("stash-env-secret: one or more writes to %s failed", ghEnv)
	}
	return nil
}
