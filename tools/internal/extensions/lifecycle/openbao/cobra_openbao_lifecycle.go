package openbao

import (
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/spf13/cobra"
)

// cobra_openbao_lifecycle.go — the cobra wiring for the four OpenBao lifecycle
// lanes. The lanes themselves are internal/baolifecycle.
//
// These came back out for the usual reason: a flag set is not a capability. What
// is different here is the SIGNATURE. Every one of these lanes took a
// `globalOpts` and read exactly one field from it, `dryRun` — so the package
// takes a bool and package main is the only thing that knows globalOpts exists.
// Four functions had been carrying a three-field struct to use a third of it.

func BaoEnsureReadyCmd() *cobra.Command {
	var region, escrowPubKey string
	var leaderTimeout, joinTimeout int
	c := &cobra.Command{
		Use:   "bao-ensure-ready",
		Short: "probe OpenBao and drive it to unsealed + a usable root token (init/wait/regen)",
		Long: "Orchestrates the OpenBao seal/token lifecycle bootstrap-openbao.yml used to\n" +
			"run as eight separate steps: probe all pods, then — on an uninitialized\n" +
			"cluster — run `bao operator init` and wait for every pod to auto-unseal\n" +
			"from the static seal key (leader first, then the retry_join'd followers);\n" +
			"on an initialized-but-sealed cluster wait for the pod(s) to self-unseal\n" +
			"after a restart; and on an initialized cluster validate the loaded root\n" +
			"token and regenerate it via quorum if a prior run revoked it. Composes the\n" +
			"same bao-init / bao-regen-root logic the individual commands run. Writes\n" +
			"available=<bool> to $GITHUB_OUTPUT (the gate downstream configure/seed steps\n" +
			"check) and re-exports the effective OPENBAO_ROOT_TOKEN to $GITHUB_ENV. Reads\n" +
			"RECOVERY_K1/2/3 + OPENBAO_ROOT_TOKEN (infra-<region> secrets) and\n" +
			"GH_TOKEN/GH_REPO (first-init persistence). On the first-init branch,\n" +
			"--escrow-pubkey-b64 is forwarded to bao-init and is validated up front so\n" +
			"--dry-run vets it before any share is minted.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunEnsureReady(cliopts.Global.DryRun, region, escrowPubKeyOrEnv(escrowPubKey),
				time.Duration(leaderTimeout)*time.Second, time.Duration(joinTimeout)*time.Second)
		},
	}
	c.Flags().StringVar(&region, "region", "", "region whose infra-<region> GHA environment holds the keys/token (required)")
	c.Flags().StringVar(&escrowPubKey, "escrow-pubkey-b64", "", "base64 of an RSA public-key PEM to encrypt the recovery shares to on first init (default $OPENBAO_ESCROW_PUBKEY_B64)")
	c.Flags().IntVar(&leaderTimeout, "leader-timeout", 180, "seconds to wait for pod-0 to report unsealed (first-init)")
	c.Flags().IntVar(&joinTimeout, "join-timeout", 300, "seconds to wait for each follower to reach initialized=true (first-init)")
	return c
}

func BaoInitCmd() *cobra.Command {
	var region, escrowPubKey string
	c := &cobra.Command{
		Use:   "bao-init",
		Short: "first-time `bao operator init`: mask, escrow or persist recovery shares, write job summary",
		Long: "Native port of init-cluster.sh (bootstrap-openbao.yml Branch A). Runs\n" +
			"`bao operator init -recovery-shares=5 -recovery-threshold=3` on pod-0. Under\n" +
			"the chart's `seal \"static\"` auto-unseal the pods unseal themselves at boot,\n" +
			"so init yields RECOVERY shares (for generate-root/rekey quorum), not unseal\n" +
			"keys. Exports OPENBAO_ROOT_TOKEN + RECOVERY_K1-3 to $GITHUB_ENV for the\n" +
			"downstream steps, and persists recovery shares 1-3 plus the root token as\n" +
			"infra-<region> environment secrets. Emits did_init=true. Requires\n" +
			"GH_TOKEN/GH_REPO (the secrets-write PAT).\n" +
			"\n" +
			"NO KEY MATERIAL IS EVER WRITTEN TO $GITHUB_STEP_SUMMARY. Masking redacts\n" +
			"logs; a job summary is rendered from a file that masking never touches, so\n" +
			"the payload this used to print handed anyone with Actions READ a full 3-of-5\n" +
			"quorum and the root token. With --escrow-pubkey-b64 the 5 shares are\n" +
			"RSA-OAEP/SHA-256-encrypted to your key (ciphertext in the summary plus\n" +
			"$RUNNER_TEMP/openbao-recovery-keys.b64); without it, shares 4 and 5 are\n" +
			"persisted as OPENBAO_RECOVERY_KEY_4/5 instead and you get no offline copy.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunInit(cliopts.Global.DryRun, region, escrowPubKeyOrEnv(escrowPubKey))
		},
	}
	c.Flags().StringVar(&region, "region", "", "region whose infra-<region> GHA environment receives the secrets (required)")
	c.Flags().StringVar(&escrowPubKey, "escrow-pubkey-b64", "", "base64 of an RSA public-key PEM to encrypt the 5 recovery shares to (default $OPENBAO_ESCROW_PUBKEY_B64)")
	return c
}

// escrowPubKeyOrEnv resolves the escrow key from the flag, falling back to
// $OPENBAO_ESCROW_PUBKEY_B64.
//
// THE ENV FALLBACK IS THE PATH THAT ACTUALLY RUNS. The delivered bootstrap
// invokes `bao-ensure-ready` from a workflow step, and a workflow passes an
// optional value far more safely through `env:` than by splicing it into a
// `run:` argv — a PEM is base64 with `+`/`/` in it, and an empty input would
// otherwise leave a bare `--escrow-pubkey-b64` consuming the next flag. The flag
// stays for the by-hand `llz ci bao-init` handle.
func escrowPubKeyOrEnv(flagValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		return flagValue
	}
	return os.Getenv("OPENBAO_ESCROW_PUBKEY_B64")
}

func BaoRegenRootCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "bao-regen-root",
		Short: "regenerate the root token via quorum if the loaded one is revoked",
		Long: "Native port of regenerate-root-if-revoked.sh. The end-of-run \"Revoke root\n" +
			"token\" step revokes the in-workflow token but can't update the GH secret,\n" +
			"so the next run loads a dead value. This validates $OPENBAO_ROOT_TOKEN via\n" +
			"`bao token lookup` and exits 0 if it works; otherwise it runs the\n" +
			"`bao operator generate-root` quorum flow with RECOVERY_K1/2/3 (keys piped\n" +
			"over stdin, never argv — under auto-unseal generate-root is authorized by\n" +
			"the recovery keys, not unseal keys), masks the new token, exports it to\n" +
			"$GITHUB_ENV for the downstream steps, and writes it back to the\n" +
			"infra-<region> OPENBAO_ROOT_TOKEN environment secret. Interactive operator\n" +
			"twin: `llz openbao regen-root`.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunRegenRootCI(cliopts.Global.DryRun, region)
		},
	}
	c.Flags().StringVar(&region, "region", "", "region whose infra-<region> GHA environment holds OPENBAO_ROOT_TOKEN (required)")
	return c
}

func BaoBreakglassCmd() *cobra.Command {
	var region, action, pubkeyB64 string
	c := &cobra.Command{
		Use:   "bao-breakglass",
		Short: "operator break-glass: generate/rotate/revoke an OpenBao root token from the recovery quorum",
		Long: "Operator front-end to the OpenBao recovery quorum, driven by\n" +
			"llz-breakglass-openbao.yml. --action selects one of:\n" +
			"  generate  regenerate a root token via `bao-regen-root` and return it\n" +
			"            RSA-OAEP-encrypted to --recipient-pubkey-b64.\n" +
			"  rotate    revoke the current root token first (so no untracked live root\n" +
			"            lingers), then regenerate + return a fresh one.\n" +
			"  revoke    revoke the current root token and delete the\n" +
			"            infra-<region>::OPENBAO_ROOT_TOKEN environment secret.\n" +
			"The token is never printed in the clear; only ciphertext leaves the job\n" +
			"(a base64 file at $RUNNER_TEMP/root-token.b64 + a job-summary block).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunBreakglass(cliopts.Global.DryRun, region, action, pubkeyB64)
		},
	}
	c.Flags().StringVar(&region, "region", "", "deployment whose infra-<region> Environment holds the recovery quorum + OPENBAO_ROOT_TOKEN (required)")
	c.Flags().StringVar(&action, "action", "", "generate | rotate | revoke (required)")
	c.Flags().StringVar(&pubkeyB64, "recipient-pubkey-b64", "", "base64 of the operator RSA public-key PEM; required for generate/rotate, ignored for revoke")
	return c
}
