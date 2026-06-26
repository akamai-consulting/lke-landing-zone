package main

// ci_ensure_secret.go implements `llz ci ensure-env-secret` — the pre-apply
// counterpart to the post-apply `stash-env-secret`. It moves the generation of a
// "generate-once, then reuse" credential OUT of Terraform state and into llz:
// instead of cluster-bootstrap's random_password generating the Loki admin
// password on the first apply (then the workflow reading it back via
// `terraform output` and stashing it), this command generates it BEFORE the
// apply, persists it to the infra-<env> Environment via `gh secret set`, and
// exports it as TF_VAR_<var> to $GITHUB_ENV so the apply consumes it as a plain
// input. Terraform keeps no copy and the apply→output→stash round-trip is gone.
//
// GitHub Actions secrets are write-only over the API, so "generate if missing"
// keys off the current value passed into the job (the <NAME> env var, sourced
// from `${{ secrets.<NAME> }}`): empty means first run → generate + persist;
// non-empty means an earlier run already stored it → reuse verbatim. Either way
// the effective value is exported as TF_VAR_<var>, so the apply step must NOT
// also define that variable (a step/job-level env: would shadow $GITHUB_ENV).

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func ciEnsureEnvSecretCmd() *cobra.Command {
	var ghEnv, name, exportTFVar string
	var genBytes int
	c := &cobra.Command{
		Use:   "ensure-env-secret",
		Short: "ensure a generate-once GitHub environment secret exists, exporting it as a TF var",
		Long: "Pre-apply counterpart to stash-env-secret. Reads the current value of the\n" +
			"--name secret from the environment (sourced from `${{ secrets.<NAME> }}`):\n" +
			"when empty (first run) it generates an alphanumeric value from --gen-bytes\n" +
			"crypto/rand bytes and persists it to the --env Environment via `gh secret\n" +
			"set`; when already set it reuses the value verbatim. The effective value is\n" +
			"::add-mask::ed and, with --export-tf-var <var>, appended to $GITHUB_ENV as\n" +
			"TF_VAR_<var> for the apply step. Auth: the ambient GH_TOKEN (a fine-grained\n" +
			"PAT with Actions + Secrets: write on this repo).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIEnsureEnvSecret(ghEnv, name, exportTFVar, genBytes)
		},
	}
	c.Flags().StringVar(&ghEnv, "env", "", "GitHub Environment to write a freshly generated secret into, e.g. infra-primary")
	c.Flags().StringVar(&name, "name", "", "secret name; also the env var read for the current value (required)")
	c.Flags().StringVar(&exportTFVar, "export-tf-var", "", "append TF_VAR_<var>=<value> to $GITHUB_ENV for the apply step")
	c.Flags().IntVar(&genBytes, "gen-bytes", 24, "crypto/rand byte count for a freshly generated value (base64 alphanumeric)")
	return c
}

func runCIEnsureEnvSecret(ghEnv, name, exportTFVar string, genBytes int) error {
	if name == "" {
		return fmt.Errorf("--name is required (the secret name and the env var holding its current value)")
	}
	value := os.Getenv(name)
	if value == "" {
		if ghEnv == "" {
			return fmt.Errorf("%s is unset and --env was not given — cannot persist a freshly generated value", name)
		}
		v, err := generateAlnumSecret(genBytes)
		if err != nil {
			return err
		}
		value = v
		if err := ghSetSecretFn(name, ghEnv, value); err != nil {
			// A failure here would regenerate a different value next run (password
			// churn), so it is fatal — same rationale as stash-env-secret.
			fmt.Fprintf(os.Stderr, "::error::'gh secret set %s --env %s' failed: %v\n", name, ghEnv, err)
			fmt.Fprintf(os.Stderr, "::error::Most common cause: the GH_TOKEN PAT lacks access to %s (Environment) or the repo (needs Actions + Secrets: write).\n", ghEnv)
			return fmt.Errorf("persist generated %s to %s: %w", name, ghEnv, err)
		}
		fmt.Printf("Generated %s and stored it on %s.\n", name, ghEnv)
	} else {
		fmt.Printf("%s already set — reusing the stored value.\n", name)
	}
	maskGHALines(value)
	if exportTFVar != "" {
		if err := appendGHAFile("GITHUB_ENV", "TF_VAR_"+exportTFVar+"="+value); err != nil {
			return fmt.Errorf("export TF_VAR_%s to $GITHUB_ENV: %w", exportTFVar, err)
		}
		fmt.Printf("Exported TF_VAR_%s for the apply step.\n", exportTFVar)
	}
	return nil
}

// generateAlnumSecret returns an alphanumeric secret derived from nBytes of
// crypto/rand — base64 with the non-alphanumeric chars (/ + =) stripped, matching
// the gen:base64:N source and keeping the value safe for HTTP basic-auth / URL
// contexts (what cluster-bootstrap's random_password.loki_admin guaranteed with
// special=false). Uses the seedRandRead seam so it is deterministic under test.
func generateAlnumSecret(nBytes int) (string, error) {
	if nBytes <= 0 {
		nBytes = 24
	}
	b := make([]byte, nBytes)
	if err := seedRandRead(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return strings.NewReplacer("/", "", "+", "", "=", "").
		Replace(base64.StdEncoding.EncodeToString(b)), nil
}
