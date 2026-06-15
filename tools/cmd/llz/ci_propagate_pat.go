package main

// ci_propagate_pat.go implements `llz ci propagate-pat` — the native port of
// llz-secret-rotation.yml's 'Probe OpenBao + write secret/linode/api-token'
// step. After a Linode PAT rotation, the new token must reach each region's
// OpenBao or every consumer reading secret/linode/api-token keeps using the
// revoked one. The write authenticates via the `secret-propagator` AppRole
// (write on secret/data/linode/api-token only) — NOT root: bootstrap revokes
// the root token at the end of every run by design.
//
// Env contract (identical to the step's env: block):
//   REGION                        — deployment being propagated to (messages)
//   OPENBAO_PROPAGATOR_ROLE_ID    — secret-propagator AppRole role_id
//   OPENBAO_PROPAGATOR_SECRET_ID  — secret-propagator AppRole secret_id
//   NEW_TOKEN                     — secrets.LINODE_API_TOKEN (re-fetched after
//                                   the create job rewrote it; a masked value
//                                   cannot cross a job boundary as an output)
//   NEW_PAT_ID                    — created PAT's id (audit label; optional)
//   NEW_TOKEN_HASH                — sha256 of the token the create job minted;
//                                   empty in propagate-only recovery mode

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
		Short: "write the rotated Linode PAT into this cluster's OpenBao via the secret-propagator AppRole",
		Long: "Native port of the 'Probe OpenBao + write secret/linode/api-token' rotation\n" +
			"step. Verifies the token in NEW_TOKEN matches the hash the create job\n" +
			"emitted (guarding against a stale GHA secret refetch silently undoing the\n" +
			"rotation), logs in with the secret-propagator AppRole, and writes\n" +
			"secret/linode/api-token over stdin so the token never appears on argv.\n" +
			"Unbootstrapped regions (no AppRole creds seeded, or no OpenBao pod) skip\n" +
			"with a summary note. Env: REGION, OPENBAO_PROPAGATOR_{ROLE_ID,SECRET_ID},\n" +
			"NEW_TOKEN, NEW_PAT_ID, NEW_TOKEN_HASH.",
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

	roleID, secretID := os.Getenv("OPENBAO_PROPAGATOR_ROLE_ID"), os.Getenv("OPENBAO_PROPAGATOR_SECRET_ID")
	if roleID == "" || secretID == "" {
		fmt.Fprintf(os.Stderr, "::warning::OPENBAO_PROPAGATOR_ROLE_ID/SECRET_ID not set in infra-%s — cannot write secret/linode/api-token\n", region)
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			fmt.Sprintf("> Skipped: `OPENBAO_PROPAGATOR_*` not seeded in `infra-%s`. Re-run bootstrap-openbao.yml to seed the secret-propagator AppRole.", region))
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

	// AppRole login: exchange role_id + secret_id for a short-lived token bound
	// to the secret-propagator policy (TTL 15m/30m, set by bootstrap-openbao).
	out, _, err := baoExecFn(rootOpenbaoPod, "", "",
		"write", "-f", "auth/approle/login",
		"role_id="+roleID, "secret_id="+secretID, "-format=json")
	var login struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &login); err != nil || jsonErr != nil || login.Auth.ClientToken == "" {
		return fmt.Errorf("AppRole login failed for secret-propagator on %s — check the OPENBAO_PROPAGATOR_SECRET_ID is current (may have expired or been rotated out of band)", region)
	}
	maskGHA(login.Auth.ClientToken)

	// Write the PAT with the AppRole-issued token, NOT root. The payload rides
	// stdin so the token never appears in argv / ps / kubectl audit logs.
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
		fmt.Sprintf("> Wrote `secret/linode/api-token` (new_pat_id=`%s`) via secret-propagator AppRole.", idLabel))
}
