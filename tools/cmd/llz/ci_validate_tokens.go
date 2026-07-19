package main

// ci_validate_tokens.go — `llz ci validate-tokens`: the CI counterpart of the
// local `llz doctor` validity probe. It reads each pipeline credential from the
// ENVIRONMENT (where CI injects the repo/infra-<env> secrets) and actively probes
// it, so a set-but-dead token — the GHCR_READ_TOKEN 403 that failed a run
// mid-bootstrap being the motivating case — fails FAST with "rotate it" instead
// of 401/403-ing deep inside a 45-minute provision.
//
// It probes two independent things per credential: VALIDITY (does it
// authenticate? — token_validate.go) and CAPABILITY (is it scoped for the job it
// exists for? — token_capability.go). The second exists because the first is not
// sufficient: an under-scoped PAT authenticates perfectly and still 403s on the
// operation, which is how a "✓ valid, expires in 77d" verdict was followed six
// minutes later by `gh secret set --env infra-prod` failing 403 — after the
// cluster was already up. See token_capability.go for that scar in full.
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
			"a later provision. Credentials with a required SCOPE are additionally probed\n" +
			"for authorization against the read-only twin of the call they later make, so\n" +
			"an under-scoped-but-valid PAT is caught here too. Absent credentials are\n" +
			"skipped (a ::notice::, not a failure) and an unreachable endpoint is a warning\n" +
			"(not the token's fault). Exit 0 when nothing is invalid, 1 when a probed\n" +
			"credential is INVALID or DENIED its required scope (unless\n" +
			"--fail-on-invalid=false). The local counterpart is `llz doctor`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIValidateTokens(failOnInvalid)
		},
	}
	c.Flags().BoolVar(&failOnInvalid, "fail-on-invalid", true,
		"exit 1 if any probed credential is invalid; =false reports only")
	return c
}

// runCIValidateTokens returns nil when nothing blocking is invalid and an error
// otherwise (cobra exits 1 on it). The ::error:: annotation stays a direct
// write: GitHub parses an annotation only at the start of a line, and a returned
// error reaches stderr behind main.go's "llz: " prefix.
func runCIValidateTokens(failOnInvalid bool) error {
	now := time.Now()
	ghcrUser := os.Getenv("GHCR_USERNAME")

	fmt.Printf("%s\n", bold("Token validity — probing pipeline credentials in the environment"))
	probed, blockingInvalid, optionalInvalid, blockingDenied := 0, 0, 0, 0
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

		// Authorization, reported as an indented child of the validity line. Asked
		// only of a credential that authenticated: a dead token has nothing to
		// authorize, and a second verdict would just bury the real cause.
		if tv.status == vInvalid {
			continue
		}
		if cr, ok := checkCapability(name, val); ok {
			if cr.status == capDenied {
				if optionalTokens[name] {
					fmt.Fprintf(os.Stderr, "::warning::%s is not authorized for its required scope but is optional — it won't block the run.\n", name)
				} else {
					blockingDenied++
					fmt.Fprintf(os.Stderr, "::error::%s: %s\n", name, capabilityHint(name))
				}
			}
			fmt.Printf("  %-30s %s\n", "  └ scope", capabilityCell(cr))
		}
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

	fmt.Printf("\nprobed %d credential(s): %d blocking-invalid, %d optional-invalid, %d scope-denied.\n",
		probed, blockingInvalid, optionalInvalid, blockingDenied)

	// Denial and invalidity are reported separately because the remediation
	// differs: an invalid token needs ROTATING, a denied one needs RE-SCOPING, and
	// telling an operator to rotate a perfectly live PAT sends them down the wrong
	// path. Both are gated by --fail-on-invalid — one switch for "report only".
	if failOnInvalid {
		switch {
		case blockingInvalid > 0 && blockingDenied > 0:
			fmt.Fprintf(os.Stderr, "::error::%d REQUIRED credential(s) are invalid and %d lack a required scope — fix both before this run proceeds.\n", blockingInvalid, blockingDenied)
			return fmt.Errorf("%d REQUIRED pipeline credential(s) are invalid and %d lack a required scope", blockingInvalid, blockingDenied)
		case blockingInvalid > 0:
			fmt.Fprintf(os.Stderr, "::error::%d REQUIRED pipeline credential(s) are invalid — rotate them before this run proceeds.\n", blockingInvalid)
			return fmt.Errorf("%d REQUIRED pipeline credential(s) are invalid — rotate them before this run proceeds", blockingInvalid)
		case blockingDenied > 0:
			fmt.Fprintf(os.Stderr, "::error::%d REQUIRED pipeline credential(s) authenticate but lack a required scope — re-scope them (NOT rotate) before this run proceeds.\n", blockingDenied)
			return fmt.Errorf("%d REQUIRED pipeline credential(s) lack a required scope — re-scope them before this run proceeds", blockingDenied)
		}
	}
	return nil
}
