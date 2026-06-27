package main

// ci_propagate_pat.go implements `llz ci propagate-pat` — the native port of
// llz-secret-rotation.yml's 'Probe OpenBao + write secret/linode/api-token'
// step. After a Linode PAT rotation, the new token must reach each region's
// OpenBao or every consumer reading secret/linode/api-token keeps using the
// revoked one. The write authenticates via the `secret-propagator` GitHub-OIDC
// (jwt) role — a short-lived, per-run, repo-bound token (NOT a long-lived
// AppRole secret_id, and NOT root: bootstrap revokes root at the end of run).
//
// Env contract:
//   REGION              — deployment being propagated to (messages)
//   GITHUB_REPOSITORY   — "<owner>/<name>" (auto-set by Actions); drives the
//                         OIDC audience that matches the jwt role's bound_audiences
//   ACTIONS_ID_TOKEN_REQUEST_URL / _TOKEN — set when the job has
//                         `permissions: id-token: write`; used to mint the OIDC JWT
//   NEW_TOKEN           — secrets.LINODE_API_TOKEN (re-fetched after the create
//                         job rewrote it; a masked value cannot cross a job
//                         boundary as an output)
//   NEW_PAT_ID          — created PAT's id (audit label; optional)
//   NEW_TOKEN_HASH      — sha256 of the token the create job minted; empty in
//                         propagate-only recovery mode

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func ciPropagatePATCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "propagate-pat",
		Short: "write the rotated Linode PAT into this cluster's OpenBao via the secret-propagator GitHub-OIDC role",
		Long: "Native port of the 'Probe OpenBao + write secret/linode/api-token' rotation\n" +
			"step. Verifies the token in NEW_TOKEN matches the hash the create job\n" +
			"emitted (guarding against a stale GHA secret refetch silently undoing the\n" +
			"rotation), mints a GitHub Actions OIDC token and exchanges it via OpenBao's\n" +
			"jwt auth (role secret-propagator), and writes secret/linode/api-token over\n" +
			"stdin so the token never appears on argv. Regions without an OpenBao pod\n" +
			"skip with a summary note. Needs `permissions: id-token: write`. Env: REGION,\n" +
			"GITHUB_REPOSITORY, ACTIONS_ID_TOKEN_REQUEST_{URL,TOKEN}, NEW_TOKEN,\n" +
			"NEW_PAT_ID, NEW_TOKEN_HASH.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIPropagatePAT() },
	}
}

func runCIPropagatePAT() error {
	region := os.Getenv("REGION")
	if err := appendGHAFile("GITHUB_STEP_SUMMARY",
		fmt.Sprintf("## Linode PAT propagation — %s", region), ""); err != nil {
		return err
	}

	ghRepo := os.Getenv("GITHUB_REPOSITORY")
	if ghRepo == "" {
		return fmt.Errorf("GITHUB_REPOSITORY is empty — cannot derive the OIDC audience for the secret-propagator jwt login")
	}
	newToken := os.Getenv("NEW_TOKEN")
	if newToken == "" {
		return fmt.Errorf("secrets.LINODE_API_TOKEN is empty — cannot propagate")
	}
	// Belt-and-suspenders: the rotate action already masked the token, but
	// re-register it here so nothing this step prints can leak it.
	maskGHA(newToken)

	// Hash check (skipped in propagate-only mode, where there is no create job
	// to source the hash from). When create did run, this detects the unlikely
	// case that the GHA secret refetch was stale and NEW_TOKEN still points at
	// the previous PAT — writing that to OpenBao would silently undo the
	// rotation.
	newPATID := os.Getenv("NEW_PAT_ID")
	if wantHash := os.Getenv("NEW_TOKEN_HASH"); wantHash != "" {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(newToken))); got != wantHash {
			return fmt.Errorf("secrets.LINODE_API_TOKEN does not match the PAT created in this run (sha256 mismatch). GHA secret refetch was stale — aborting before the OpenBao write")
		}
		fmt.Printf("Token hash verified (matches new_pat_id=%s).\n", newPATID)
	} else {
		fmt.Printf("Propagate-only mode: no hash to verify. Writing secrets.LINODE_API_TOKEN as-is to %s OpenBao.\n", region)
		if err := appendGHAFile("GITHUB_STEP_SUMMARY", "> Propagate-only: hash check skipped."); err != nil {
			return err
		}
	}

	if !kExists("-n", openbaoNS, "get", "pod", rootOpenbaoPod) {
		fmt.Fprintf(os.Stderr, "::warning::%s not found on %s — skipping propagation\n", rootOpenbaoPod, region)
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			fmt.Sprintf("> Skipped: OpenBao pod not found on `%s`.", region))
	}

	// Mint a GitHub Actions OIDC token for the owner's audience, then exchange it
	// via OpenBao's jwt auth for a short-lived token bound to the secret-propagator
	// policy. No long-lived secret_id, no `gh secret set` round-trip: the role is
	// repo-bound (bound_claims.repository) in `llz ci bao-configure`.
	oidcToken, err := githubActionsOIDCToken(oidcAudienceForRepo(ghRepo), nil)
	if err != nil {
		return fmt.Errorf("mint GitHub OIDC token for %s: %w (does the job set `permissions: id-token: write`?)", region, err)
	}
	maskGHA(oidcToken)
	out, _, err := baoExecFn(rootOpenbaoPod, "", "",
		"write", "-f", "auth/jwt/login",
		"role=secret-propagator", "jwt="+oidcToken, "-format=json")
	var login struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &login); err != nil || jsonErr != nil || login.Auth.ClientToken == "" {
		return fmt.Errorf("jwt login failed for secret-propagator on %s — check the jwt role's bound_claims/bound_audiences match this repo (run `llz ci bao-configure`)", region)
	}
	maskGHA(login.Auth.ClientToken)

	// Write the PAT with the OIDC-issued (jwt-login) token, NOT root. The payload
	// rides stdin so the token never appears in argv / ps / kubectl audit logs.
	payload, err := json.Marshal(map[string]string{"token": newToken})
	if err != nil {
		return err
	}
	if _, errOut, err := baoExecFn(rootOpenbaoPod, login.Auth.ClientToken, string(payload),
		"kv", "put", "secret/linode/api-token", "-"); err != nil {
		return fmt.Errorf("bao kv put secret/linode/api-token on %s: %s", region, strings.TrimSpace(errOut))
	}

	idLabel := newPATID
	if idLabel == "" {
		idLabel = "<unknown — propagate-only>"
	}
	fmt.Printf("Wrote secret/linode/api-token to %s OpenBao (new_pat_id=%s).\n", region, idLabel)
	return appendGHAFile("GITHUB_STEP_SUMMARY",
		fmt.Sprintf("> Wrote `secret/linode/api-token` (new_pat_id=`%s`) via secret-propagator GitHub-OIDC role.", idLabel))
}
