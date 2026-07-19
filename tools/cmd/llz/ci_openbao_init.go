package main

// ci_openbao_init.go — `llz ci bao-init` and `llz ci bao-regen-root`, the
// credential-lifecycle half of the openbao CI family (see ci_openbao.go):
// native ports of init-cluster.sh and regenerate-root-if-revoked.sh.
// bao-regen-root is the NON-INTERACTIVE twin of the operator-facing
// `llz openbao regen-root` (regenroot.go): same quorum flow, but the keys
// come from the RECOVERY_K1/2/3 env (infra-<region> secrets) instead of a
// terminal prompt, and the refreshed token is written straight back to the
// GHA environment. Both reuse regenroot.go's baoExec + JSON parse helpers.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// ghSetSecretFn writes a GitHub Actions environment secret via
// `gh secret set <name> --env <ghEnv>` with the value piped over stdin (never
// argv-visible). gh resolves auth + repo from the ambient GH_TOKEN/GH_REPO —
// the same contract the shell scripts ran under. Seamed for tests.
var ghSetSecretFn = func(name, ghEnv, value string) error {
	return ghSecretSetStdin(name, ghEnv, value)
}

// ── bao-init ──────────────────────────────────────────────────────────────────

// baoInitResult is the payload of `bao operator init -format=json`. Under an
// auto-unseal seal (the chart configures `seal "static"`) init yields RECOVERY
// keys, not unseal keys: the seal mechanism unseals every pod at boot, and the
// recovery shares exist only to authorize `operator generate-root` /
// `operator rekey` quorum flows. They CANNOT decrypt the root key, so they are
// not sufficient to unseal if the static key is lost — that key must be backed
// up offline (see runCIBaoInit's summary banner).
type baoInitResult struct {
	RootToken       string   `json:"root_token"`
	RecoveryKeysB64 []string `json:"recovery_keys_b64"`
}

// parseBaoInit validates the init payload: a root token plus at least the 5
// recovery shares requested. Anything less means init half-failed and nothing
// below may proceed (the shares are generated exactly once).
func parseBaoInit(s string) (baoInitResult, error) {
	var r baoInitResult
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return r, fmt.Errorf("operator init returned unparseable JSON: %w", err)
	}
	if r.RootToken == "" || len(r.RecoveryKeysB64) < 5 {
		return r, fmt.Errorf("operator init payload incomplete (root=%v, %d recovery shares)", r.RootToken != "", len(r.RecoveryKeysB64))
	}
	return r, nil
}

func ciBaoInitCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "bao-init",
		Short: "first-time `bao operator init`: mask, persist recovery keys + root, write job summary",
		Long: "Native port of init-cluster.sh (bootstrap-openbao.yml Branch A). Runs\n" +
			"`bao operator init -recovery-shares=5 -recovery-threshold=3` on pod-0. Under\n" +
			"the chart's `seal \"static\"` auto-unseal the pods unseal themselves at boot,\n" +
			"so init yields RECOVERY shares (for generate-root/rekey quorum), not unseal\n" +
			"keys. Masks all six values, writes the full init payload to\n" +
			"$GITHUB_STEP_SUMMARY FIRST (the shares are generated exactly once and cannot\n" +
			"be recovered — capturing them must not be gated on gh/network success),\n" +
			"exports OPENBAO_ROOT_TOKEN + RECOVERY_K1-3 to $GITHUB_ENV for the downstream\n" +
			"steps, and persists recovery keys 1-3 plus the root token as infra-<region>\n" +
			"environment secrets. Emits did_init=true. Requires GH_TOKEN/GH_REPO (the\n" +
			"secrets-write PAT).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIBaoInit(gopts, region) },
	}
	c.Flags().StringVar(&region, "region", "", "region whose infra-<region> GHA environment receives the secrets (required)")
	return c
}

func runCIBaoInit(g globalOpts, region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) would run `bao operator init` and persist recovery keys to infra-"+region)
		return nil
	}
	pod := openbaoPodNames[0]
	initOut, errOut, err := baoExecFn(pod, "", "",
		"operator", "init", "-recovery-shares=5", "-recovery-threshold=3", "-format=json")
	if err != nil {
		return fmt.Errorf("operator init on %s: %s", pod, strings.TrimSpace(firstNonEmpty(errOut, initOut)))
	}
	res, err := parseBaoInit(initOut)
	if err != nil {
		return err
	}

	// Mask everything before any other output can echo it.
	maskGHA(res.RootToken)
	for _, k := range res.RecoveryKeysB64 {
		maskGHA(k)
	}

	// Job summary first — before any step that can fail (see Long help).
	if err := appendGHAFile("GITHUB_STEP_SUMMARY",
		"## OpenBao Initialized — Save These Keys Now",
		"",
		"**OPERATOR ACTION REQUIRED:**",
		"Copy all 5 recovery keys and the root token to secure offline storage",
		"immediately. They will not be shown again.",
		"Back up the cluster's 32-byte static unseal key offline TOO — recovery keys",
		"authorize generate-root but CANNOT decrypt the root key, so losing the static",
		"key loses the data.",
		"Delete the `OPENBAO_ROOT_TOKEN` environment secret once bootstrap completes.",
		"",
		"```json",
		strings.TrimSpace(initOut),
		"```"); err != nil {
		return err
	}

	if err := appendGHAFile("GITHUB_ENV",
		"OPENBAO_ROOT_TOKEN="+res.RootToken,
		"RECOVERY_K1="+res.RecoveryKeysB64[0],
		"RECOVERY_K2="+res.RecoveryKeysB64[1],
		"RECOVERY_K3="+res.RecoveryKeysB64[2]); err != nil {
		return err
	}
	// Also export to the PROCESS env. A standalone `bao-init` step relies on the
	// $GITHUB_ENV write above (GitHub Actions injects it into the next step), but
	// the `bao-ensure-ready` orchestrator runs init + regen-root in ONE process —
	// the generate-root path reads RECOVERY_K1/2/3 (and the availability gate
	// reads OPENBAO_ROOT_TOKEN) via os.Getenv, which the file write does not
	// satisfy.
	os.Setenv("OPENBAO_ROOT_TOKEN", res.RootToken)
	os.Setenv("RECOVERY_K1", res.RecoveryKeysB64[0])
	os.Setenv("RECOVERY_K2", res.RecoveryKeysB64[1])
	os.Setenv("RECOVERY_K3", res.RecoveryKeysB64[2])

	// Recovery keys 1-3 as environment secrets for the generate-root quorum
	// (bao-regen-root); the root token too, for two reasons: (1) the configure
	// preflight prints the sha256 of secrets.OPENBAO_ROOT_TOKEN so the operator
	// can spot GHA-vs-cluster mismatch on the NEXT bootstrap — without persisting
	// now, a prior cluster's stale token is what the audit reads; (2) `llz openbao
	// regen-root` against this cluster needs the GHA-stored value to be CURRENT.
	// The summary banner tells the operator to delete it after bootstrap; a
	// leftover fails clean on the next cold bootstrap's preflight.
	ghEnv := "infra-" + region
	for i, key := range res.RecoveryKeysB64[:3] {
		if err := ghSetSecretFn(fmt.Sprintf("OPENBAO_RECOVERY_KEY_%d", i+1), ghEnv, key); err != nil {
			return err
		}
	}
	if err := ghSetSecretFn("OPENBAO_ROOT_TOKEN", ghEnv, res.RootToken); err != nil {
		return err
	}

	fmt.Printf("OpenBao initialized; recovery keys 1-3 + root token persisted to %s.\n", ghEnv)
	return appendGHAFile("GITHUB_OUTPUT", "did_init=true")
}

// ── bao-regen-root ────────────────────────────────────────────────────────────

func ciBaoRegenRootCmd() *cobra.Command {
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
		RunE: func(_ *cobra.Command, _ []string) error { return runCIBaoRegenRoot(gopts, region) },
	}
	c.Flags().StringVar(&region, "region", "", "region whose infra-<region> GHA environment holds OPENBAO_ROOT_TOKEN (required)")
	return c
}

func runCIBaoRegenRoot(g globalOpts, region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) would validate $OPENBAO_ROOT_TOKEN and regenerate via quorum if revoked")
		return nil
	}
	pod := openbaoPodNames[0]

	// The generate-root flow requires an unsealed leader; if the cluster is
	// still sealed surface that explicitly instead of a confusing API error
	// halfway through. (The bash needed a jq-`//`-operator workaround here;
	// the typed parse sidesteps it.)
	statusOut, _, _ := baoExecFn(pod, "", "", "status", "-format=json")
	st, ok := parseBaoPodStatus(statusOut)
	if !ok || st.Sealed {
		state := "true"
		if !ok {
			state = "unknown"
		}
		fmt.Fprintf(os.Stderr, "::error::pod-0 sealed-status check returned '%s' (expected 'false'). generate-root requires an unsealed leader. Check the unseal steps above and the cluster's bao status.\n", state)
		return fmt.Errorf("%s is not unsealed (sealed=%s)", pod, state)
	}

	// `token lookup` (no args = self) succeeds for any valid token; the -self
	// flag isn't supported on every OpenBao version.
	//
	// err != nil used to mean "revoked", full stop — so a kubectl exec failure, a
	// container not yet ready, or a konnectivity drop that outlasted the retries
	// all took the regeneration branch: burn a recovery-key quorum, mint a SECOND
	// live root token, and overwrite the infra-<region> env secret. The original
	// token stays valid and untracked — a privileged credential nobody holds a
	// reference to. The sealed-status probe directly above already draws this
	// distinction ("unknown" vs sealed); this one now does too, and stops rather
	// than guessing, because both wrong guesses here are expensive.
	if token := os.Getenv("OPENBAO_ROOT_TOKEN"); token != "" {
		_, stderr, err := baoExecFn(pod, token, "", "token", "lookup")
		switch {
		case err == nil:
			fmt.Println("Root token is valid — skipping regeneration.")
			return nil
		case tokenLookupRejected(stderr):
			fmt.Println("Root token is invalid (revoked from prior run?) — regenerating via quorum.")
		default:
			fmt.Fprintf(os.Stderr, "::error::could not validate OPENBAO_ROOT_TOKEN on %s: the lookup did not "+
				"come back with an answer (%v: %s). This is NOT evidence the token was revoked, and "+
				"regenerating on a guess would mint a second root token while leaving the current one live "+
				"and untracked. Fix the exec path (pod Ready? konnectivity up?) and re-run.\n",
				pod, err, strings.TrimSpace(stderr))
			return fmt.Errorf("root-token validation on %s was inconclusive: %w", pod, err)
		}
	} else {
		fmt.Println("No OPENBAO_ROOT_TOKEN set — regenerating via quorum.")
	}

	keys, err := recoveryKeysFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::Cannot regenerate — OPENBAO_RECOVERY_KEY_{1,2,3} env secrets not set on infra-%s.\n", region)
		return err
	}

	// Cancel any in-progress attempt (idempotent), then start fresh.
	_, _, _ = baoExecFn(pod, "", "", "operator", "generate-root", "-cancel")
	initOut, errOut, err := baoExecFn(pod, "", "", "operator", "generate-root", "-init", "-format=json")
	if err != nil {
		return fmt.Errorf("generate-root -init: %s", strings.TrimSpace(firstNonEmpty(errOut, initOut)))
	}
	nonce, otp := parseGenRootInit(initOut)
	if nonce == "" || otp == "" {
		return fmt.Errorf("generate-root -init returned no nonce/otp: %s", strings.TrimSpace(initOut))
	}
	maskGHA(otp)

	// Submit the 3 keys against the same nonce; the final submission returns
	// encoded_token. Keys ride stdin (`-`), not argv.
	var encoded string
	for i, key := range keys {
		out, errOut, err := baoExecFn(pod, "", key+"\n",
			"operator", "generate-root", "-nonce="+nonce, "-format=json", "-")
		if err != nil {
			return fmt.Errorf("generate-root rejected key %d/3: %s", i+1, strings.TrimSpace(firstNonEmpty(errOut, out)))
		}
		if complete, _, _, enc := parseGenRootStep(out); complete {
			encoded = enc
		}
	}
	if encoded == "" {
		fmt.Fprintln(os.Stderr, "::error::Quorum didn't produce encoded_token. Check unseal-key correctness.")
		return fmt.Errorf("quorum completed without an encoded_token")
	}

	// Decode the encoded token using the OTP (in-pod, like regenroot.go).
	decodeOut, _, _ := baoExecFn(pod, "", "", "operator", "generate-root", "-decode="+encoded, "-otp="+otp, "-format=json")
	newRoot := parseTokenField(decodeOut)
	if newRoot == "" { // older bao prints a bare token
		bare, _, _ := baoExecFn(pod, "", "", "operator", "generate-root", "-decode="+encoded, "-otp="+otp)
		newRoot = strings.TrimSpace(bare)
	}
	if newRoot == "" {
		return fmt.Errorf("generate-root decode produced no token")
	}
	maskGHA(newRoot)

	// Update env for downstream steps + the GH secret for the next run. The
	// os.Setenv mirror lets the in-process `bao-ensure-ready` availability gate
	// read the REGENERATED token (not the stale one it loaded) via os.Getenv;
	// a standalone step gets it from the $GITHUB_ENV injection.
	if err := appendGHAFile("GITHUB_ENV", "OPENBAO_ROOT_TOKEN="+newRoot); err != nil {
		return err
	}
	os.Setenv("OPENBAO_ROOT_TOKEN", newRoot)
	if err := ghSetSecretFn("OPENBAO_ROOT_TOKEN", "infra-"+region, newRoot); err != nil {
		return err
	}
	fmt.Printf("New root token written to infra-%s::OPENBAO_ROOT_TOKEN.\n", region)
	return nil
}
