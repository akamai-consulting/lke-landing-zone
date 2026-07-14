package main

// ci_validate_tokens.go — `llz ci validate-tokens`: the CI counterpart of the
// local `llz doctor` validity probe. It reads each pipeline credential from the
// ENVIRONMENT (where CI injects the repo/infra-<env> secrets) and actively probes
// it, so a set-but-dead token — the GHCR_READ_TOKEN 403 that failed a run
// mid-bootstrap being the motivating case — fails FAST with "rotate it" instead
// of 401/403-ing deep inside a 45-minute provision.
//
// This is what closes the gap the local wizard can't: GitHub exposes secret
// values only inside the job, never to `llz doctor` on a laptop. Wire it as an
// early preflight in a workflow that already has the credentials in env. Probe
// logic is shared with token_validate.go (probeToken); this file is the env read
// + exit-code shell. Exit 0 all-valid (or only warnings/unreachable), 1 if any
// probed credential is INVALID and --fail-on-invalid (default).

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// validatableTokens is the ordered set of pipeline credentials this verb probes
// from the environment. Only auth-bearing tokens with a probe (kindFor != none)
// belong here; each is checked only when its env var is set.
var validatableTokens = []string{
	"LINODE_API_TOKEN",
	"LINODE_DNS_TOKEN",
	"OPENBAO_SECRETS_WRITE_TOKEN",
	"APL_VALUES_REPO_TOKEN",
	"E2E_DISPATCH_TOKEN",
	"GHCR_READ_TOKEN",
}

// optionalTokens never block the run when invalid — only WARN. These are the
// credentials that aren't required for a stock instance: GHCR_READ_TOKEN (the
// charts are public, and ghcrPullToken falls back to anonymous) and
// LINODE_DNS_TOKEN (DNS-01 certs are opt-in). An invalid REQUIRED token (Linode
// API, the GitHub PATs) is a hard fail — it WILL break the run downstream.
var optionalTokens = map[string]bool{
	"GHCR_READ_TOKEN":  true,
	"LINODE_DNS_TOKEN": true,
}

func ciValidateTokensCmd() *cobra.Command {
	var failOnInvalid bool
	c := &cobra.Command{
		Use:   "validate-tokens",
		Short: "probe each pipeline credential in the environment and fail fast on an invalid/expired one",
		Long: "Actively validates the pipeline credentials present in the environment —\n" +
			"Linode PATs (GET /v4/profile), GitHub PATs (token-expiration probe), and the\n" +
			"GHCR read token (GHCR token endpoint) — so a set-but-expired/revoked/mistyped\n" +
			"credential fails HERE with a clear 'rotate it' rather than 401/403-ing deep in\n" +
			"a later provision. Absent credentials are skipped (a ::notice::, not a failure)\n" +
			"and an unreachable endpoint is a warning (not the token's fault). Exit 0 when\n" +
			"nothing is invalid, 1 when a probed credential is INVALID (unless\n" +
			"--fail-on-invalid=false). The local counterpart is `llz doctor`.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			os.Exit(runCIValidateTokens(failOnInvalid))
			return nil
		},
	}
	c.Flags().BoolVar(&failOnInvalid, "fail-on-invalid", true,
		"exit 1 if any probed credential is invalid; =false reports only")
	return c
}

func runCIValidateTokens(failOnInvalid bool) int {
	now := time.Now()
	ghcrUser := os.Getenv("GHCR_USERNAME")

	fmt.Printf("%s\n", bold("Token validity — probing pipeline credentials in the environment"))
	probed, blockingInvalid, optionalInvalid := 0, 0, 0
	for _, name := range validatableTokens {
		val := os.Getenv(name)
		if val == "" {
			fmt.Printf("  %-30s %s\n", name, dim("– not set — skipped"))
			continue
		}
		// Keep the secret value out of any downstream log capture.
		fmt.Fprintf(os.Stderr, "::add-mask::%s\n", val)
		tv := probeToken(name, val, ghcrUser, now)
		probed++
		suffix := ""
		if tv.status == vInvalid {
			if optionalTokens[name] {
				optionalInvalid++
				suffix = dim("  (optional — warning only)")
				fmt.Fprintf(os.Stderr, "::warning::%s is invalid but optional — it won't block the run; rotate or unset it.\n", name)
			} else {
				blockingInvalid++
			}
		}
		fmt.Printf("  %-30s %s%s\n", name, validityCell(tv), suffix)
	}

	// OBJ state-bucket key pair (REQUIRED) — validated together via SigV4.
	if ak, sk := os.Getenv("TF_STATE_ACCESS_KEY"), os.Getenv("TF_STATE_SECRET_KEY"); ak != "" && sk != "" {
		fmt.Fprintf(os.Stderr, "::add-mask::%s\n", ak)
		fmt.Fprintf(os.Stderr, "::add-mask::%s\n", sk)
		tv := probeS3Pair(ak, sk, os.Getenv("TF_STATE_ENDPOINT"), os.Getenv("TF_STATE_BUCKET"))
		probed++
		if tv.status == vInvalid {
			blockingInvalid++
		}
		fmt.Printf("  %-30s %s\n", "TF_STATE_ACCESS_KEY/SECRET", validityCell(tv))
	}

	fmt.Printf("\nprobed %d credential(s): %d blocking-invalid, %d optional-invalid.\n", probed, blockingInvalid, optionalInvalid)
	if blockingInvalid > 0 && failOnInvalid {
		fmt.Fprintf(os.Stderr, "::error::%d REQUIRED pipeline credential(s) are invalid — rotate them before this run proceeds.\n", blockingInvalid)
		return 1
	}
	return 0
}
