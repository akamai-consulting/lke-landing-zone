package baolifecycle

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghsecret"
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

func RunInit(dryRun bool, region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
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

	// Job summary first — before any step that can fail (see Long help).
	if err := ghaout.Append("GITHUB_STEP_SUMMARY",
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

	fmt.Printf("OpenBao initialized; recovery keys 1-3 + root token persisted to %s.\n", ghEnv)
	return ghaout.Append("GITHUB_OUTPUT", "did_init=true")
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

	// Submit the 3 keys against the same nonce; the final submission returns
	// encoded_token. Keys ride stdin (`-`), not argv.
	var encoded string
	for i, key := range keys {
		out, errOut, err := baoread.ExecFn(pod, "", key+"\n",
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
