package openbao

// ci_openbao_init.go — `llz ci bao-init` and `llz ci bao-regen-root`, the
// credential-lifecycle half of the openbao CI family (see ci_openbao.go):
// native ports of init-cluster.sh and regenerate-root-if-revoked.sh.
// bao-regen-root is the NON-INTERACTIVE twin of the operator-facing
// `llz openbao regen-root` (regenroot.go): same quorum flow, but the keys
// come from the RECOVERY_K1/2/3 env (infra-<region> secrets) instead of a
// terminal prompt, and the refreshed token is written straight back to the
// GHA environment. Both reuse regenroot.go's baoExec + JSON parse helpers.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghsecret"
)

type baoInitResult struct {
	RootToken       string   `json:"root_token"`
	RecoveryKeysB64 []string `json:"recovery_keys_b64"`
}

// ParseInit validates the init payload: a root token plus at least the 5
// recovery shares requested. Anything less means init half-failed and nothing
// below may proceed (the shares are generated exactly once).
func ParseInit(s string) (baoInitResult, error) {
	var r baoInitResult
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return r, fmt.Errorf("operator init returned unparseable JSON: %w", err)
	}
	if r.RootToken == "" || len(r.RecoveryKeysB64) < 5 {
		return r, fmt.Errorf("operator init payload incomplete (root=%v, %d recovery shares)", r.RootToken != "", len(r.RecoveryKeysB64))
	}
	return r, nil
}

func RunInit(dryRun bool, region, escrowPubKeyB64 string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}

	// Parse + validate the escrow key BEFORE `operator init` runs. The recovery
	// shares are generated exactly once and there is no second chance to deliver
	// them, so a malformed key must fail while there is still nothing to lose —
	// the same ordering, for the same reason, as bao-breakglass refusing to burn a
	// quorum regeneration on a bad key.
	var escrow *rsa.PublicKey
	if strings.TrimSpace(escrowPubKeyB64) != "" {
		var err error
		if escrow, err = ParseRecipientRSAPubKey(escrowPubKeyB64); err != nil {
			return fmt.Errorf("escrow public key rejected (nothing was initialized): %w", err)
		}
	}

	if dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) would run `bao operator init` and persist recovery keys to infra-"+region)
		return nil
	}
	pod := baoread.PodNames[0]
	initOut, errOut, err := baoread.ExecFn(pod, "", "",
		"operator", "init", "-recovery-shares=5", "-recovery-threshold=3", "-format=json")
	if err != nil {
		return fmt.Errorf("operator init on %s: %s", pod, strings.TrimSpace(firstNonEmpty(errOut, initOut)))
	}
	res, err := ParseInit(initOut)
	if err != nil {
		return err
	}

	// Mask everything before any other output can echo it.
	ghsecret.Mask(res.RootToken)
	for _, k := range res.RecoveryKeysB64 {
		ghsecret.Mask(k)
	}

	// THE JOB SUMMARY IS NOT A PRIVATE CHANNEL, and this step used to treat it as
	// one: it wrote the raw `operator init` payload — the root token and ALL FIVE
	// recovery shares — into a fenced block, on the reasoning that the shares are
	// minted once and capturing them must not be gated on gh/network success.
	// The durability reasoning was right; the channel was wrong. ghsecret.Mask
	// redacts LOGS, and a job summary is rendered from a file that masking never
	// touches, so anyone with Actions **read** on the instance repo could read a
	// 3-of-5 threshold's worth of shares — five of five, in fact — and the root
	// token beside them, and reconstitute full admin. Actions-read is a much wider
	// grant than environment-secret write, which is the boundary every other copy
	// of these values sits behind.
	//
	// So nothing derived from the init payload is written in the clear on either
	// path below. What replaces it depends on whether an escrow key was supplied:
	//
	//   escrow    all five shares, RSA-OAEP/SHA-256-encrypted to the operator's
	//             key — ciphertext in the summary (durable, and useless to a log
	//             reader) plus a $RUNNER_TEMP file for artifact upload.
	//   fallback  no key material anywhere; shares 4 and 5 are persisted as
	//             infra-<region> environment secrets so they still exist.
	//
	// WHY LOSING A SHARE IS SURVIVABLE, and why the fallback is not a durability
	// regression dressed up: the quorum is 3-of-5 and shares 1-3 are persisted on
	// both paths, hard-failing if they cannot be. Shares 4 and 5 are redundancy
	// for a lost 1-3, never the difference between break-glass working and not.
	// That is what lets the fallback warn on a failed 4/5 write instead of wedging
	// a bootstrap over a loss no retry can repair.
	//
	// The shares are encrypted ONE PER BLOCK rather than as a single JSON payload:
	// RSA-OAEP/SHA-256 on a 2048-bit key carries 190 bytes, and five base64 shares
	// do not fit. Per-share blocks keep the primitive identical to break-glass's
	// instead of introducing a hybrid envelope for one call site.
	if escrow != nil {
		if err := deliverEscrowedShares(region, escrow, res.RecoveryKeysB64); err != nil {
			return err
		}
	}

	if err := ghaout.Append("GITHUB_ENV",
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
		if err := ghsecret.SetFn(fmt.Sprintf("OPENBAO_RECOVERY_KEY_%d", i+1), ghEnv, key); err != nil {
			return err
		}
	}
	if err := ghsecret.SetFn("OPENBAO_ROOT_TOKEN", ghEnv, res.RootToken); err != nil {
		return err
	}

	if escrow == nil {
		persistFallbackShares(ghEnv, res.RecoveryKeysB64[3:5])
	}
	if err := appendInitSummary(region, escrow != nil); err != nil {
		return err
	}

	fmt.Printf("OpenBao initialized; recovery keys 1-3 + root token persisted to %s.\n", ghEnv)
	return ghaout.Append("GITHUB_OUTPUT", "did_init=true")
}

// deliverEscrowedShares encrypts each recovery share to the operator's key and
// delivers ONLY ciphertext: one base64 block per line in
// $RUNNER_TEMP/openbao-recovery-keys.b64 (for artifact upload) and the same
// blocks inline in the job summary.
//
// INLINE IN THE SUMMARY TOO, deliberately. The artifact is uploaded by a separate
// workflow step that a caller can omit, mis-wire, or lose to a cancelled run,
// and these shares do not come round again. Ciphertext costs nothing to
// duplicate: it is unreadable without the operator's offline private key, which
// is the whole reason this path exists.
func deliverEscrowedShares(region string, escrow *rsa.PublicKey, shares []string) error {
	blocks := make([]string, 0, len(shares))
	for i, share := range shares {
		ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, escrow, []byte(share), nil)
		if err != nil {
			// A share is ~45 bytes, far under the single-block OAEP limit for a
			// >= 2048-bit key, and ParseRecipientRSAPubKey already rejected
			// anything smaller — so this is unreachable short of a broken key.
			return fmt.Errorf("RSA-OAEP encryption of recovery share %d failed: %w", i+1, err)
		}
		blocks = append(blocks, base64.StdEncoding.EncodeToString(ct))
	}

	outDir := os.Getenv("RUNNER_TEMP")
	if outDir == "" {
		outDir = os.TempDir()
	}
	outPath := filepath.Join(outDir, "openbao-recovery-keys.b64")
	if err := os.WriteFile(outPath, []byte(strings.Join(blocks, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("write encrypted recovery shares to %s: %w", outPath, err)
	}
	fmt.Printf("5 encrypted recovery shares written to %s (ciphertext only — useless without your offline private key).\n", outPath)

	lines := []string{
		"## OpenBao Initialized — Escrow These Shares Now",
		"",
		"All 5 recovery shares, RSA-OAEP / SHA-256-encrypted to the public key you supplied.",
		"They are minted once and are not recoverable from OpenBao. Decrypt locally with your",
		"OFFLINE private key and store the plaintext outside GitHub:",
		"",
		"```bash",
		"cat > openbao-recovery-keys.b64 <<'CIPHER_EOF'",
	}
	lines = append(lines, blocks...)
	lines = append(lines,
		"CIPHER_EOF",
		"while read -r b; do printf '%s' \"$b\" | base64 -d \\",
		"  | openssl pkeyutl -decrypt -inkey escrow-priv.pem \\",
		"      -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256; echo; \\",
		"done < openbao-recovery-keys.b64",
		"```",
		"",
		"Shares are in index order (1-5). The threshold is **3 of 5**, so KEEP AT LEAST 3 —",
		"a copy of fewer authorizes nothing.",
		fmt.Sprintf("> Shares 1-3 are ALSO live in `infra-%s` as `OPENBAO_RECOVERY_KEY_1/2/3` (write-only in the UI),", region),
		"> which is what `bao-breakglass` and `bao-regen-root` use. Shares 4 and 5 exist ONLY in the",
		"> ciphertext above — losing your private key loses them, though not the quorum.",
	)
	return ghaout.Append("GITHUB_STEP_SUMMARY", lines...)
}

// persistFallbackShares stores shares 4 and 5 as environment secrets when no
// escrow key was supplied, so values that are minted exactly once still exist
// somewhere.
//
// IT DOES NOT WIDEN THE BLAST RADIUS. The threshold is 3 and shares 1-3 are
// already in this environment, so anyone who can read it already holds a
// complete quorum; adding 4 and 5 changes nothing about what a compromise of it
// yields. (That co-location is a real finding — see docs/secrets.md — but it is
// the escrow path above, not this one, that closes it.)
//
// WARNS RATHER THAN FAILS, and that asymmetry with shares 1-3 is deliberate. A
// failed 1-3 write is fatal because it costs the quorum. Shares 4 and 5 are
// redundancy: losing them leaves break-glass fully working, and by the time this
// runs the shares are already minted, so failing the bootstrap would wedge it
// over a loss that re-running cannot repair.
func persistFallbackShares(ghEnv string, shares []string) {
	for i, key := range shares {
		name := fmt.Sprintf("OPENBAO_RECOVERY_KEY_%d", i+4)
		if err := ghsecret.SetFn(name, ghEnv, key); err != nil {
			fmt.Printf("::warning::could not persist %s to %s: %v. Shares 1-3 are stored and the 3-of-5 quorum is intact, so break-glass still works — but this share is now LOST (they are minted once). Re-run with an escrow public key on the next cold bootstrap to get an offline copy.\n", name, ghEnv, err)
			continue
		}
		fmt.Printf("Recovery share %d persisted to %s (no escrow key was supplied).\n", i+4, name)
	}
}

// appendInitSummary writes the operator-facing banner. It carries NO key
// material on either path — see the block in RunInit — so it says where the
// shares went and what the operator still has to do.
func appendInitSummary(region string, escrowed bool) error {
	lines := []string{
		"### OpenBao — what to do next",
		"",
		"**OPERATOR ACTION REQUIRED:**",
	}
	if escrowed {
		lines = append(lines,
			"Decrypt the recovery shares above with your offline private key and store them",
			"outside GitHub. Nothing here holds a plaintext copy of shares 4 and 5.",
		)
	} else {
		lines = append(lines,
			"No escrow public key was supplied, so the recovery shares were NOT delivered to you —",
			fmt.Sprintf("all 5 are held as `infra-%s` environment secrets (`OPENBAO_RECOVERY_KEY_1`-`5`),", region),
			"which are write-only in the UI and cannot be read back. Break-glass works, but you have",
			"**no offline escrow copy**, so losing that environment loses the quorum. To obtain one you",
			"must re-shard with `bao operator rekey`, which mints new shares and invalidates these.",
			"Supply an escrow public key on the next cold bootstrap to avoid this.",
		)
	}
	return ghaout.Append("GITHUB_STEP_SUMMARY", append(lines,
		"",
		"Back up the cluster's 32-byte static unseal key offline TOO — recovery shares",
		"authorize generate-root but CANNOT decrypt the root key, so losing the static",
		"key loses the data.",
		fmt.Sprintf("Delete the `OPENBAO_ROOT_TOKEN` secret on `infra-%s` once bootstrap completes.", region),
	)...)
}

// ── bao-regen-root ────────────────────────────────────────────────────────────

func RunRegenRootCI(dryRun bool, region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	if dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) would validate $OPENBAO_ROOT_TOKEN and regenerate via quorum if revoked")
		return nil
	}
	pod := baoread.PodNames[0]

	// The generate-root flow requires an unsealed leader; if the cluster is
	// still sealed surface that explicitly instead of a confusing API error
	// halfway through. (The bash needed a jq-`//`-operator workaround here;
	// the typed parse sidesteps it.)
	statusOut, _, _ := baoread.ExecFn(pod, "", "", "status", "-format=json")
	st, ok := baoread.ParsePodStatus(statusOut)
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
		_, stderr, err := baoread.ExecFn(pod, token, "", "token", "lookup")
		switch {
		case err == nil:
			fmt.Println("Root token is valid — skipping regeneration.")
			return nil
		case baoread.TokenLookupRejected(stderr):
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

	keys, err := baoread.RecoveryKeysFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::Cannot regenerate — OPENBAO_RECOVERY_KEY_{1,2,3} env secrets not set on infra-%s.\n", region)
		return err
	}

	// Cancel any in-progress attempt (idempotent), then start fresh.
	_, _, _ = baoread.ExecFn(pod, "", "", "operator", "generate-root", "-cancel")
	initOut, errOut, err := baoread.ExecFn(pod, "", "", "operator", "generate-root", "-init", "-format=json")
	if err != nil {
		return fmt.Errorf("generate-root -init: %s", strings.TrimSpace(firstNonEmpty(errOut, initOut)))
	}
	nonce, otp := parseGenRootInit(initOut)
	if nonce == "" || otp == "" {
		return fmt.Errorf("generate-root -init returned no nonce/otp: %s", strings.TrimSpace(initOut))
	}
	ghsecret.Mask(otp)

	// Submit keys against the same nonce until the quorum COMPLETES; the completing
	// submission returns encoded_token. Keys ride stdin (`-`), not argv.
	//
	// BREAKING ON `complete` IS LOAD-BEARING, and this loop used to run all three
	// unconditionally. The unseal THRESHOLD is not necessarily three: at a threshold
	// of two, the second key completes generate-root and OpenBao mints the root
	// token right there. Submitting a third key against a nonce that has already
	// completed is an error, so the loop returned "generate-root rejected key 3/3" —
	// after the token existed and before anything decoded it.
	//
	// The result is the worst outcome this command has: a LIVE root token nobody
	// holds, no record that it was created, and an error message pointing the
	// operator at unseal-key correctness — the one thing that was fine.
	// regenroot.go's interactive loop has always exited on `complete`
	// (`for progress := 1; encoded == ""; progress++`); this is the same rule, and
	// the two paths differing on it is how it went unnoticed.
	var encoded string
	for i, key := range keys {
		out, errOut, err := baoread.ExecFn(pod, "", key+"\n",
			"operator", "generate-root", "-nonce="+nonce, "-format=json", "-")
		if err != nil {
			return fmt.Errorf("generate-root rejected key %d/%d: %s", i+1, len(keys), strings.TrimSpace(firstNonEmpty(errOut, out)))
		}
		if complete, _, _, enc := parseGenRootStep(out); complete {
			encoded = enc
			break
		}
	}
	if encoded == "" {
		fmt.Fprintln(os.Stderr, "::error::Quorum didn't produce encoded_token. Check unseal-key correctness.")
		return fmt.Errorf("quorum completed without an encoded_token")
	}

	// Decode the encoded token using the OTP (in-pod, like regenroot.go).
	decodeOut, _, _ := baoread.ExecFn(pod, "", "", "operator", "generate-root", "-decode="+encoded, "-otp="+otp, "-format=json")
	newRoot := parseTokenField(decodeOut)
	if newRoot == "" { // older bao prints a bare token
		bare, _, _ := baoread.ExecFn(pod, "", "", "operator", "generate-root", "-decode="+encoded, "-otp="+otp)
		newRoot = strings.TrimSpace(bare)
	}
	if newRoot == "" {
		return fmt.Errorf("generate-root decode produced no token")
	}
	ghsecret.Mask(newRoot)

	// Update env for downstream steps + the GH secret for the next run. The
	// os.Setenv mirror lets the in-process `bao-ensure-ready` availability gate
	// read the REGENERATED token (not the stale one it loaded) via os.Getenv;
	// a standalone step gets it from the $GITHUB_ENV injection.
	if err := ghaout.Append("GITHUB_ENV", "OPENBAO_ROOT_TOKEN="+newRoot); err != nil {
		return err
	}
	os.Setenv("OPENBAO_ROOT_TOKEN", newRoot)
	if err := ghsecret.SetFn("OPENBAO_ROOT_TOKEN", "infra-"+region, newRoot); err != nil {
		return err
	}
	fmt.Printf("New root token written to infra-%s::OPENBAO_ROOT_TOKEN.\n", region)
	return nil
}
