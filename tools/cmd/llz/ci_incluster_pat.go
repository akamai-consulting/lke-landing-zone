package main

// ci_incluster_pat.go implements the narrow IN-CLUSTER Linode PAT lifecycle —
// the tier-2 split of docs/designs/linode-pat-dns-consolidation.md. The broad
// provisioning PAT (LINODE_API_TOKEN) becomes CI/Terraform-only; every
// in-cluster Linode consumer (volume-labeler, the cred-rotator's minting
// credential, the cidr-firewall discover/controller, and — via the
// kyverno-dns-rotating-token mutation — apl-core's DNS-01 webhook and
// ExternalDNS) reads ONE rotating, narrowly-scoped token from
// secret/linode/api-token instead. The path is REPURPOSED, not renamed, so the
// existing ExternalSecrets, OpenBao policies, and the secret-propagator role
// keep working unchanged.
//
// Two commands, one lifecycle:
//
//   llz ci mint-bootstrap-pat    — bootstrap twin of mint-bootstrap-objkeys
//     (llz-bootstrap-openbao.yml, root token live): mints the narrow PAT with
//     the broad provisioning PAT, verifies it, seeds secret/linode/api-token.
//     Skip-if-present: a re-bootstrap never clobbers a rotation-minted token.
//     (On a pre-split cluster the seeded value is the OLD broad PAT — also
//     skipped; the next monthly rotation converges the path to a narrow token.)
//
//   llz ci rotate-incluster-pat  — the monthly rotation step
//     (llz-secret-rotation.yml, replaces the retired `llz ci propagate-pat`).
//     Mints a fresh narrow PAT per region IN the per-region job — the token
//     never crosses a job boundary and never touches a GitHub secret (the old
//     propagate flow had to round-trip the broad PAT through the GHA secret
//     because ::add-mask::'d values cannot ride job outputs). Verifies it,
//     writes it via the secret-propagator GitHub-OIDC role, then drains older
//     same-labeled siblings past the grace window (keep-newest — a sibling
//     that failed verification is never the newest, so it drains too).
//
// The broad PAT can mint sub-tokens because it carries account:read_write;
// whether a scoped PAT may self-mint is unconfirmed (design §5), so rotation
// deliberately stays in CI with the broad PAT as the minting credential
// (outcome B) instead of adding a PAT entry to the in-cluster rotator table.
//
// Env (both): LINODE_API_TOKEN — the BROAD provisioning PAT (minting cred).
// mint-bootstrap-pat additionally uses OPENBAO_ROOT_TOKEN (via baoKVPutFn);
// rotate-incluster-pat additionally uses REGION, GITHUB_REPOSITORY and
// ACTIONS_ID_TOKEN_REQUEST_{URL,TOKEN} (`permissions: id-token: write`).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

const (
	// inclusterPATScopes is the union of what in-cluster workloads need, and
	// nothing else — no lke/vpc:rw/nodebalancers (Terraform's concerns), no
	// account:read_write (token minting stays with the broad CI PAT):
	//   domains:read_write       — DNS-01 solver webhook + ExternalDNS (the
	//                              kyverno-dns-rotating-token mutation points
	//                              apl-core's two DNS ExternalSecrets here)
	//   object_storage:read_write — the in-cluster cred-rotator mints/drains
	//                              the Loki/Harbor S3 keys with this token
	//   volumes:read_write       — linode-volume-labeler
	//   linodes:read_only        — cidr-firewall discover (instance lookup)
	//   vpcs:read_only           — cidr-firewall discover (subnet CIDR)
	//   firewall:read_write      — cidr-firewall controller reconciles rules
	inclusterPATScopes       = "domains:read_write object_storage:read_write volumes:read_write linodes:read_only vpcs:read_only firewall:read_write"
	inclusterPATValidityDays = 90 // same ceiling the broad PAT's 90-day policy enforces
	inclusterPATGraceDays    = 7  // ESO refresh is 1-5m; a week covers any straggling consumer
)

// inclusterPATLabel is the Linode-side token label — also the drain target
// (keep-newest, like every rotated credential). Per-region: each region's
// OpenBao gets its own token, so revoking one region never breaks another.
func inclusterPATLabel(region string) string { return "llz-incluster-" + region }

// mintVerifiedInclusterPAT mints the narrow PAT with the broad provisioning
// token and confirms the NEW token authenticates (GET /v4/profile) before the
// caller writes it anywhere. A token that fails verification is left for the
// drain (it can never be the newest verified sibling).
func mintVerifiedInclusterPAT(ctx context.Context, mint patAPI, region string) (id uint64, token string, err error) {
	expiry := linode.FmtLinodeTS(rotatorNow().Unix() + inclusterPATValidityDays*linode.DaySecs)
	resp, err := mint.CreateProfileToken(ctx, inclusterPATLabel(region), inclusterPATScopes, expiry)
	if err != nil {
		return 0, "", fmt.Errorf("mint in-cluster PAT %s: %w", inclusterPATLabel(region), err)
	}
	id, ok := cli.AsUint64(resp["id"])
	if !ok {
		return 0, "", fmt.Errorf("mint in-cluster PAT: create response missing .id")
	}
	token, ok = resp["token"].(string)
	if !ok || token == "" {
		return 0, "", fmt.Errorf("mint in-cluster PAT: create response missing .token")
	}
	maskGHA(token)
	if err := linodeRotatorClient(token).Verify(ctx); err != nil {
		return 0, "", fmt.Errorf("verify freshly-minted in-cluster PAT (id=%d): %w — leaving it for the next drain", id, err)
	}
	return id, token, nil
}

func ciMintBootstrapPATCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "mint-bootstrap-pat",
		Short: "mint the first narrow in-cluster Linode PAT and seed secret/linode/api-token",
		Long: "Bootstrap-time twin of rotate-incluster-pat: mints the narrow in-cluster PAT\n" +
			"(domains/object_storage/volumes rw + linodes/vpcs ro + firewall rw) with the\n" +
			"broad provisioning PAT and seeds secret/linode/api-token — the single rotating\n" +
			"token every in-cluster Linode consumer reads. Idempotent: an already-seeded\n" +
			"path is skipped, so a re-bootstrap never clobbers a rotation-minted token.\n" +
			"Reads LINODE_API_TOKEN (mint), OPENBAO_ROOT_TOKEN (seed).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIMintBootstrapPAT(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment whose in-cluster PAT to mint (required)")
	return c
}

func runCIMintBootstrapPAT(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	minting := os.Getenv("LINODE_API_TOKEN")
	if minting == "" {
		return fmt.Errorf("LINODE_API_TOKEN must be set (mints the in-cluster PAT)")
	}
	// Idempotency: a seeded path means a rotation (or an earlier bootstrap)
	// owns a live token — minting again would strand it until the next drain.
	if baoKVGetField("secret/linode/api-token", "token") != "" {
		fmt.Println("secret/linode/api-token already seeded — skipping mint (rotation owns it).")
		return nil
	}
	id, token, err := mintVerifiedInclusterPAT(context.Background(), newPATRotatorClient(minting), region)
	if err != nil {
		return err
	}
	if err := baoKVPutFn("secret/linode/api-token", map[string]string{
		"token": token,
		// rotated_at/pat_id: the rotation-age SLA clock + the audit handle —
		// same convention as the mint-bootstrap-objkeys seeds.
		"rotated_at": strconv.FormatInt(rotatorNow().Unix(), 10),
		"pat_id":     strconv.FormatUint(id, 10),
	}); err != nil {
		return fmt.Errorf("seed secret/linode/api-token: %w", err)
	}
	fmt.Printf("Minted in-cluster PAT %s (id=%d) and seeded secret/linode/api-token.\n", inclusterPATLabel(region), id)
	return appendGHAFile("GITHUB_STEP_SUMMARY",
		fmt.Sprintf("Minted in-cluster PAT `%s` (id=`%d`) and seeded `secret/linode/api-token`.", inclusterPATLabel(region), id))
}

func ciRotateInclusterPATCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate-incluster-pat",
		Short: "mint a fresh narrow in-cluster PAT, write it to this region's OpenBao, drain old siblings",
		Long: "Replaces `llz ci propagate-pat`: instead of round-tripping the BROAD PAT\n" +
			"through a GitHub secret into every cluster, each region's job mints its own\n" +
			"NARROW in-cluster PAT (label llz-incluster-<region>) with the broad token,\n" +
			"verifies it, writes secret/linode/api-token via the secret-propagator\n" +
			"GitHub-OIDC role (payload on stdin, never argv), and drains older same-labeled\n" +
			"siblings past the grace window. The token never crosses a job boundary and\n" +
			"never touches a GitHub secret. Regions without an OpenBao pod skip with a\n" +
			"summary note. Needs `permissions: id-token: write`. Env: REGION,\n" +
			"LINODE_API_TOKEN (broad, mints), GITHUB_REPOSITORY,\n" +
			"ACTIONS_ID_TOKEN_REQUEST_{URL,TOKEN}.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIRotateInclusterPAT() },
	}
}

func runCIRotateInclusterPAT() error {
	region := os.Getenv("REGION")
	if err := appendGHAFile("GITHUB_STEP_SUMMARY",
		fmt.Sprintf("## In-cluster Linode PAT rotation — %s", region), ""); err != nil {
		return err
	}
	minting := os.Getenv("LINODE_API_TOKEN")
	if minting == "" {
		return fmt.Errorf("LINODE_API_TOKEN is empty — cannot mint the in-cluster PAT")
	}
	maskGHA(minting)

	// Probe BEFORE minting: an unbootstrapped region (no OpenBao pod) must not
	// accumulate orphan tokens on every monthly run.
	if !kExists("-n", openbaoNS, "get", "pod", rootOpenbaoPod) {
		fmt.Fprintf(os.Stderr, "::warning::%s not found on %s — skipping in-cluster PAT rotation\n", rootOpenbaoPod, region)
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			fmt.Sprintf("> Skipped: OpenBao pod not found on `%s`.", region))
	}

	ctx := context.Background()
	client := newPATRotatorClient(minting)
	id, token, err := mintVerifiedInclusterPAT(ctx, client, region)
	if err != nil {
		return err
	}
	if err := secretPropagatorKVPut(region, map[string]string{
		"token":      token,
		"rotated_at": strconv.FormatInt(rotatorNow().Unix(), 10),
		"pat_id":     strconv.FormatUint(id, 10),
	}); err != nil {
		return err
	}
	fmt.Printf("Wrote secret/linode/api-token to %s OpenBao (new_pat_id=%d).\n", region, id)
	if err := appendGHAFile("GITHUB_STEP_SUMMARY",
		fmt.Sprintf("> Wrote `secret/linode/api-token` (new_pat_id=`%d`, label `%s`) via secret-propagator GitHub-OIDC role.",
			id, inclusterPATLabel(region))); err != nil {
		return err
	}
	// Drain older same-labeled siblings past the grace window. Keep-newest
	// keeps the token just written; consumers re-sync via ESO well inside the
	// grace window. Drain failure is non-fatal by design of the monthly cadence
	// (the next run retries) — but surface it, or leaked tokens hide forever.
	if err := runCredentialsPATRevokeOld(ctx, client, true, inclusterPATLabel(region), inclusterPATGraceDays); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::drain of old %s tokens failed: %v (next monthly run retries)\n", inclusterPATLabel(region), err)
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			fmt.Sprintf("> Drain of older `%s` siblings failed (non-fatal): %v", inclusterPATLabel(region), err))
	}
	return nil
}

// secretPropagatorKVPut writes fields to secret/linode/api-token through the
// secret-propagator GitHub-OIDC (jwt) role — a short-lived, per-run,
// repo-bound token (NOT a long-lived AppRole secret_id, and NOT root). The
// payload rides stdin so the token never appears in argv / ps / kubectl audit
// logs. Lifted verbatim from the retired `llz ci propagate-pat`.
func secretPropagatorKVPut(region string, fields map[string]string) error {
	ghRepo := os.Getenv("GITHUB_REPOSITORY")
	if ghRepo == "" {
		return fmt.Errorf("GITHUB_REPOSITORY is empty — cannot derive the OIDC audience for the secret-propagator jwt login")
	}
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

	payload, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if _, errOut, err := baoExecFn(rootOpenbaoPod, login.Auth.ClientToken, string(payload),
		"kv", "put", "secret/linode/api-token", "-"); err != nil {
		return fmt.Errorf("bao kv put secret/linode/api-token on %s: %s", region, strings.TrimSpace(errOut))
	}
	return nil
}
