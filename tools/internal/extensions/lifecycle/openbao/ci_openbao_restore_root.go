package openbao

// ci_openbao_restore_root.go — the root token the restore-openbao workflow mints
// for itself, and `llz ci bao-revoke-root`, which kills it.
//
// WHY NOT bao-regen-root. That verb is bootstrap's: it mints from the same
// recovery quorum, then exports the token to $GITHUB_ENV and WRITES IT BACK to
// infra-<region>::OPENBAO_ROOT_TOKEN, because the bootstrap job hands it to a
// dozen later steps and a later bootstrap reuses it. A restore needs neither, and
// the write-back is exactly what the restore workflow exists to avoid: a root
// token parked in GitHub outlives the run, trips LLZCredentialRootTokenParked, and
// is one more copy to revoke. So the quorum half is shared
// (generateRootPod + generateRootQuorum in ci_openbao_init.go — the standby and
// sealed lessons, the break-on-complete rule) and the persistence half is not:
// here the token lives in this process and in ONE 0600 file under $RUNNER_TEMP,
// which the job's always() step reads to revoke it and then deletes. $RUNNER_TEMP
// is per-job and wiped when the job ends; nothing here touches $GITHUB_ENV, a
// step output, or a GitHub secret, and the workflow is not even handed the
// secrets-write PAT that could.
//
// THE FILE IS ALSO THE BACKSTOP'S ONLY MEMORY. The restore verb revokes nothing
// itself at exit, deliberately: a process killed by a timeout or a cancel runs
// no defer. The always() step does run, and it can revoke only what it can find.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghsecret"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

// RestoreRootTokenFileName is the token file's name under $RUNNER_TEMP. The
// workflow's configure step reads the same path; a test holds the two together.
const RestoreRootTokenFileName = "openbao-restore-root.token"

// RestoreRootTokenFile is where the restore workflow keeps its root token.
func RestoreRootTokenFile() string {
	dir := os.Getenv("RUNNER_TEMP")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, RestoreRootTokenFileName)
}

// mintEphemeralRoot regenerates a root token from the recovery quorum and records
// it in file — nowhere else.
//
// If file already holds a token (the restore verb mints twice: once to restore,
// once on the RESTORED store, whose token table no longer knows the first), that
// one is revoked first, or proven already gone. An inconclusive answer stops the
// mint: replacing the file would orphan a token that may still be live, the exact
// failure bao-regen-root's inconclusive-lookup rule exists to prevent.
func mintEphemeralRoot(region, file string) (string, error) {
	if prev, err := readTokenFile(file); err != nil {
		return "", err
	} else if prev != "" {
		if _, err := revokeRootToken(prev); err != nil {
			return "", fmt.Errorf("the root token this run minted earlier could not be revoked or shown to be gone, "+
				"so a second one is not minted over it: %w", err)
		}
	}
	pod, err := generateRootPod()
	if err != nil {
		return "", err
	}
	token, err := generateRootQuorum(pod, region)
	if err != nil {
		return "", err
	}
	// Record it BEFORE verifying it: from here on it exists, so the backstop must
	// be able to find it even if the lookup below fails.
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("record the minted root token for the revoke step (%s): %w — revoke it by hand: it is live", file, err)
	}
	out, stderr, err := rootBao().Run(pod, token, "", "token", "lookup", "-format=json")
	if err != nil {
		return "", fmt.Errorf("the minted root token failed its self-lookup: %s", strings.TrimSpace(firstNonEmpty(stderr, out)))
	}
	if !openbao.PoliciesIncludeRoot(out) {
		return "", fmt.Errorf("the minted token is valid but not root")
	}
	// The accessor is not a credential — it cannot authenticate — but it is what
	// revokes a token nobody holds any more, so it goes in the log.
	fmt.Printf("Root token minted from the infra-%s recovery quorum on %s (accessor %s). Kept in this job only; revoked by the job's final step.\n",
		region, pod, tokenAccessor(out))
	return token, nil
}

// revokeOutcome is what revokeRootToken found.
type revokeOutcome int

const (
	// tokenAlreadyGone: OpenBao rejected the token — revoked, or (after a restore)
	// absent from the restored token table.
	tokenAlreadyGone revokeOutcome = iota
	// tokenRevoked: it was live, and is not any more.
	tokenRevoked
)

// revokeRootToken revokes token, or proves it is already gone. Success means
// OpenBao itself answered "rejected" for it afterwards; a lookup that got no
// answer is an error, never "gone".
func revokeRootToken(token string) (revokeOutcome, error) {
	var lastErr error
	for _, pod := range revokePods() {
		_, stderr, err := rootBao().Run(pod, token, "", "token", "lookup")
		switch {
		case err != nil && baoread.TokenLookupRejected(stderr):
			return tokenAlreadyGone, nil
		case err != nil:
			lastErr = fmt.Errorf("%s: token lookup got no answer (%v: %s)", pod, err, strings.TrimSpace(stderr))
			continue
		}
		if _, stderr, err := rootBao().Run(pod, token, "", "token", "revoke", "-self"); err != nil {
			lastErr = fmt.Errorf("%s: token revoke -self: %v: %s", pod, err, strings.TrimSpace(stderr))
			continue
		}
		_, stderr, err = rootBao().Run(pod, token, "", "token", "lookup")
		if err != nil && baoread.TokenLookupRejected(stderr) {
			return tokenRevoked, nil
		}
		lastErr = fmt.Errorf("%s: the token still answers a lookup after `token revoke -self`", pod)
	}
	if lastErr == nil {
		lastErr = errors.New("no OpenBao pod to ask")
	}
	return 0, lastErr
}

// rootBao is the token-lifecycle handle, built from the restore binding's own
// grants (secret-custody: `token revoke` is admin, `token lookup` a read) rather
// than the raw exec seam. Built per call, so it follows the seam a test installs.
func rootBao() capability.BaoAdminHandle { return capability.For(restoreBinding()).BaoAdmin }

// revokePods is where a revoke is sent: every pod, in order. Token calls are
// authenticated, so a standby forwards them — but after a restore leadership may
// be moving and one pod may be restarting, and a single fixed pod would turn that
// into an orphaned token.
var revokePods = func() []string { return baoread.PodNames }

// RunRevokeRoot is `llz ci bao-revoke-root`: revoke the token in file and delete
// the file. No file means this run minted nothing, which is success.
func RunRevokeRoot(file string) error {
	token, err := readTokenFile(file)
	if err != nil {
		return err
	}
	if token == "" {
		fmt.Println("No root token was minted by this run — nothing to revoke.")
		_ = os.Remove(file)
		return nil
	}
	ghsecret.Mask(token)
	outcome, err := revokeRootToken(token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::the root token this run minted could NOT be revoked or shown to be gone (%v). "+
			"It may still be live, and root tokens do not expire. Revoke it by its accessor (printed by the step that minted it) "+
			"with `llz openbao exec -- token revoke -accessor <accessor>` under a root token from `breakglass-openbao.yml`, "+
			"then revoke that one with action=revoke.\n", err)
		return fmt.Errorf("root token revocation was inconclusive: %w", err)
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete %s: %w", file, err)
	}
	switch outcome {
	case tokenRevoked:
		fmt.Println("✓ the root token this run minted is revoked.")
	default:
		fmt.Println("✓ the root token this run minted is already gone (a restore replaces the token table) — nothing left to revoke.")
	}
	return nil
}

// storedRootLive reports whether token — infra-<region>::OPENBAO_ROOT_TOKEN — is
// accepted by the RESTORED store. It normally holds the token the last bootstrap
// revoked; a snapshot taken while that token (or a break-glass one) was live
// brings it back to life. ok=false means the answer was inconclusive.
func storedRootLive(token string) (live, ok bool) {
	for _, pod := range revokePods() {
		_, stderr, err := rootBao().Run(pod, token, "", "token", "lookup")
		switch {
		case err == nil:
			return true, true
		case baoread.TokenLookupRejected(stderr):
			return false, true
		}
	}
	return false, false
}

func readTokenFile(file string) (string, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", file, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// tokenAccessor reads data.accessor from `bao token lookup -format=json`.
func tokenAccessor(lookupJSON string) string {
	var v struct {
		Data struct {
			Accessor string `json:"accessor"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(lookupJSON), &v) != nil || v.Data.Accessor == "" {
		return "(unknown)"
	}
	return v.Data.Accessor
}

// BaoRevokeRootCmd is `llz ci bao-revoke-root`.
func BaoRevokeRootCmd() *cobra.Command {
	var file string
	c := &cobra.Command{
		Use:   "bao-revoke-root",
		Short: "revoke the root token the restore-openbao workflow minted, and delete its file",
		Long: "The always() tail of llz-restore-openbao.yml. Reads the root token `llz ci\n" +
			"openbao-restore` minted from the recovery quorum (--token-file, default\n" +
			"$RUNNER_TEMP/" + RestoreRootTokenFileName + "), revokes it — or proves OpenBao no longer\n" +
			"knows it, which is the normal case after a restore — and deletes the file. No\n" +
			"file means nothing was minted. Fails when the answer is inconclusive, because\n" +
			"root tokens do not expire.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if file == "" {
				file = RestoreRootTokenFile()
			}
			return RunRevokeRoot(file)
		},
	}
	c.Flags().StringVar(&file, "token-file", "", "file holding the token (default $RUNNER_TEMP/"+RestoreRootTokenFileName+")")
	return c
}
