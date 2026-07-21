package main

// ci_bao_breakglass.go implements `llz ci bao-breakglass` — the operator
// front-end to the OpenBao recovery quorum, driven by the break-glass workflow
// (instance-template/.github/workflows/llz-breakglass-openbao.yml). A root token
// is ephemeral by design: bootstrap mints one, uses it, and REVOKES it for
// hygiene. What survives is the 3-of-5 recovery quorum (OPENBAO_RECOVERY_KEY_1/2/3,
// held as infra-<region> environment secrets), which authorizes
// `operator generate-root`. This turns that quorum back into a usable token on
// demand, three ways:
//
//   generate  regenerate a root token via bao-regen-root, return it ENCRYPTED
//   rotate    revoke the current root token FIRST, then regenerate + return a fresh one
//   revoke    revoke the current root token and delete infra-<region>::OPENBAO_ROOT_TOKEN
//
// The logic lives here (not as inline workflow bash) so the input validation,
// recipient-key parsing, and RSA-OAEP delivery are unit-tested Go — same reason
// bao-ensure-ready / clear-cluster-secrets were ported off shell.
//
// SECURITY: a root token is full admin and run logs/summaries are readable by
// anyone with Actions access, so the token is NEVER printed in the clear — the
// operator supplies an RSA public key and the token is returned RSA-OAEP/SHA-256
// encrypted; only ciphertext (a base64 file + a job-summary block) leaves the job.
// RSA-OAEP with SHA-256 for both the label hash and MGF1 matches the documented
// `openssl pkeyutl -decrypt -pkeyopt rsa_oaep_md:sha256` decrypt path.
//
// Doing the whole flow in ONE process also sidesteps the GitHub Actions env
// footgun the workflow used to fight: bao-regen-root exports the fresh token to
// $GITHUB_ENV, but a static job/step `env:` binding of OPENBAO_ROOT_TOKEN is
// re-applied over it for later steps and would shadow that write. Here the
// effective token is read from the same process's os.Getenv immediately after
// regeneration, so there is nothing to shadow.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func ciBaoBreakglassCmd() *cobra.Command {
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
			return runCIBaoBreakglass(gopts, region, action, pubkeyB64)
		},
	}
	c.Flags().StringVar(&region, "region", "", "deployment whose infra-<region> Environment holds the recovery quorum + OPENBAO_ROOT_TOKEN (required)")
	c.Flags().StringVar(&action, "action", "", "generate | rotate | revoke (required)")
	c.Flags().StringVar(&pubkeyB64, "recipient-pubkey-b64", "", "base64 of the operator RSA public-key PEM; required for generate/rotate, ignored for revoke")
	return c
}

func runCIBaoBreakglass(g globalOpts, region, action, pubkeyB64 string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	switch action {
	case "generate", "rotate", "revoke":
	default:
		return fmt.Errorf("unknown action %q (expected generate | rotate | revoke)", action)
	}

	// Parse + validate the recipient key up front so we fail BEFORE burning a
	// quorum regeneration on a bad key.
	var recipient *rsa.PublicKey
	if action != "revoke" {
		if strings.TrimSpace(pubkeyB64) == "" {
			return fmt.Errorf("--recipient-pubkey-b64 is required for action=%s (base64 of your RSA public-key PEM, >= 2048-bit)", action)
		}
		var err error
		if recipient, err = parseRecipientRSAPubKey(pubkeyB64); err != nil {
			return err
		}
	}

	if g.dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) would %s the infra-%s root token via the recovery quorum\n", action, region)
		return nil
	}

	// ── revoke / rotate: kill the CURRENT root token first ──────────────────────
	// For rotate this must precede regeneration: regenerating while the old token
	// were still valid would leave a live, untracked root behind — the exact
	// anti-pattern bao-regen-root guards against. Revoking first makes the
	// subsequent lookup a definite "revoked", so regen takes cleanly.
	if action == "revoke" || action == "rotate" {
		breakglassRevokeCurrent(region)
	}

	// ── revoke: delete the stored secret so no live root lingers in GitHub ──────
	if action == "revoke" {
		return breakglassDeleteStored(region)
	}

	// ── generate / rotate: regenerate a root token via the recovery quorum ──────
	// bao-regen-root validates the loaded OPENBAO_ROOT_TOKEN and, if it is dead
	// (the normal case, and always so for rotate after the revoke above),
	// regenerates via the quorum — updating this process's env, $GITHUB_ENV, and
	// infra-<region>::OPENBAO_ROOT_TOKEN. If the loaded token is somehow still
	// valid it skips regeneration and leaves that value in place.
	if err := runCIBaoRegenRoot(g, region); err != nil {
		return err
	}

	// Effective token: the fresh value on the regen path, or the still-valid
	// stored value on the skip path — either way, whatever os.Getenv holds now
	// (same process, so no $GITHUB_ENV / job-env shadowing to reason about).
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		return fmt.Errorf("no root token to deliver — regeneration produced nothing and no valid stored token was found")
	}
	maskGHA(token)

	return breakglassEncryptAndDeliver(region, action, recipient, token)
}

// breakglassRevokeCurrent revokes whatever OPENBAO_ROOT_TOKEN currently holds
// (the infra-<region> stored value). Best-effort: an empty or already-dead token
// is a warning, never fatal — an already-revoked stored token is the normal
// steady state, and revoke must still proceed to delete the secret.
func breakglassRevokeCurrent(region string) {
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		fmt.Printf("No OPENBAO_ROOT_TOKEN stored on infra-%s — nothing to revoke.\n", region)
		return
	}
	if _, _, err := baoExecFn(openbaoPodNames[0], token, "", "token", "revoke", "-self"); err != nil {
		fmt.Println("::warning::token revoke -self failed — the stored token may already be dead. Continuing.")
		return
	}
	fmt.Println("Current root token revoked.")
}

// breakglassDeleteStored deletes infra-<region>::OPENBAO_ROOT_TOKEN so no live
// root lingers in GitHub. Best-effort delete (already-absent / lacking scope is a
// warning), then a job-summary line.
func breakglassDeleteStored(region string) error {
	ghEnv := "infra-" + region
	if err := ghDeleteSecretFn("OPENBAO_ROOT_TOKEN", ghEnv); err != nil {
		fmt.Printf("::warning::Could not delete %s::OPENBAO_ROOT_TOKEN (already absent, or the OPENBAO_SECRETS_WRITE_TOKEN PAT lacks Environments admin). Verify it is gone manually: %v\n", ghEnv, err)
	} else {
		fmt.Printf("Deleted %s::OPENBAO_ROOT_TOKEN.\n", ghEnv)
	}
	return appendGHAFile("GITHUB_STEP_SUMMARY",
		fmt.Sprintf("## OpenBao break-glass — revoke (%s)", region),
		"",
		fmt.Sprintf("Current root token revoked and `%s::OPENBAO_ROOT_TOKEN` deleted.", ghEnv),
	)
}

// breakglassEncryptAndDeliver RSA-OAEP/SHA-256-encrypts the token to the
// recipient key, writes the base64 ciphertext to $RUNNER_TEMP/root-token.b64
// (uploaded as a short-retention artifact), and appends a decrypt-recipe block to
// the job summary. Only ciphertext ever leaves the job.
func breakglassEncryptAndDeliver(region, action string, recipient *rsa.PublicKey, token string) error {
	ciphertext, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, recipient, []byte(token), nil)
	if err != nil {
		// The token is short (< 96 bytes), well under the single-block OAEP limit
		// for a >= 2048-bit key, so a failure here means the key is too small.
		return fmt.Errorf("RSA-OAEP encryption failed (is the recipient key >= 2048-bit?): %w", err)
	}
	b64 := base64.StdEncoding.EncodeToString(ciphertext)

	outDir := os.Getenv("RUNNER_TEMP")
	if outDir == "" {
		outDir = os.TempDir()
	}
	outPath := filepath.Join(outDir, "root-token.b64")
	if err := os.WriteFile(outPath, []byte(b64), 0o600); err != nil {
		return fmt.Errorf("write encrypted token to %s: %w", outPath, err)
	}
	fmt.Printf("Encrypted root token written to %s (ciphertext only — useless without your offline private key).\n", outPath)

	return appendGHAFile("GITHUB_STEP_SUMMARY",
		fmt.Sprintf("## OpenBao break-glass root token — %s (%s)", region, action),
		"",
		"Encrypted to your RSA public key (RSA-OAEP / SHA-256). Decrypt locally with your OFFLINE private key:",
		"",
		"```bash",
		"cat > root-token.b64 <<'CIPHER_EOF'",
		b64,
		"CIPHER_EOF",
		"base64 -d root-token.b64 \\",
		"  | openssl pkeyutl -decrypt -inkey bg-priv.pem \\",
		"      -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256; echo",
		"```",
		"",
		fmt.Sprintf("> The plaintext token is ALSO live in `infra-%s` :: `OPENBAO_ROOT_TOKEN` (write-only in the UI).", region),
		"> Root tokens do NOT expire. When the incident is over, re-run with **action=revoke** to kill it and delete that secret.",
	)
}

// parseRecipientRSAPubKey decodes base64(PEM) into an RSA public key, rejecting a
// pasted private key, a non-PEM/non-RSA key, and anything under 2048-bit. The
// input matches the docs: `base64 < bg-pub.pem | tr -d '\n'`.
func parseRecipientRSAPubKey(b64 string) (*rsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("--recipient-pubkey-b64 is not valid base64: %w", err)
	}
	if strings.Contains(string(der), "PRIVATE KEY") {
		return nil, fmt.Errorf("that is a PRIVATE key — paste the PUBLIC key half (bg-pub.pem)")
	}
	block, _ := pem.Decode(der)
	if block == nil {
		return nil, fmt.Errorf("--recipient-pubkey-b64 did not decode to a PEM block (expected an RSA public-key PEM)")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("recipient key is not a valid PKIX public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("recipient key is not RSA (RSA-OAEP delivery requires an RSA key)")
	}
	if rsaPub.Size() < 256 { // 256 bytes == 2048-bit
		return nil, fmt.Errorf("recipient RSA key is %d-bit; must be >= 2048-bit", rsaPub.Size()*8)
	}
	return rsaPub, nil
}
