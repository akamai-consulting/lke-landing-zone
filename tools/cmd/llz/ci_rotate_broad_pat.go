package main

// ci_rotate_broad_pat.go implements `llz ci rotate-broad-pat` — the in-cluster
// rotator for the BROAD Linode CI/TF PAT (the account:read_write LINODE_API_TOKEN
// every workflow + Terraform reads). It reverses the former "the broad PAT is
// CI/TF-only, never in-cluster" boundary: a dedicated, infrequent CronJob (NOT the
// always-on reconciler, which holds only the narrow PAT) now owns the rotation.
//
// SECURITY: this is the one in-cluster workload that holds an account:read_write
// Linode token (to mint replacements) AND a GitHub token with Actions env-secrets
// write (to publish the new value). Both arrive via ESO from OpenBao, scoped to this
// CronJob's ServiceAccount. Keeping it off the reconciler keeps that standing
// privilege minimal + isolated. See docs/designs/credential-single-pane.md.
//
// FLOW (order is load-bearing — nothing old is revoked until the new token is live
// everywhere a consumer reads it):
//   1. mint a fresh broad PAT (account:read_write, 90d) with the CURRENT broad token
//   2. VERIFY it authenticates (GET /v4/profile) — a bad mint drains nothing
//   3. write it to OpenBao (secret/linode/broad-pat) so the cluster's own copy (and
//      the next run's LINODE_TOKEN, via ESO) update
//   4. write it back to each infra-<deployment> GitHub ENVIRONMENT secret
//      (LINODE_API_TOKEN), sealed-box — the copies the workflows actually read
//   5. revoke OLDER same-labeled PATs, keeping the newest + anything inside the grace
//      window, so the token CI is actively using is never pulled out from under it
// Dry-run unless --apply.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/spf13/cobra"
)

const (
	// broadPATScopes MUST include account:read_write (to mint successor PATs) — the
	// scope the narrow in-cluster PAT deliberately withholds. Mirrors the scope set
	// the CI create-linode-pat job used.
	broadPATScopes       = "linodes:read_write object_storage:read_write lke:read_write firewall:read_write vpc:read_write volumes:read_write nodebalancers:read_write events:read_only account:read_write"
	broadPATValidityDays = 90
	// broadPATBaoPath is the cluster's own copy of the broad token; ESO syncs it to
	// the rotator's LINODE_TOKEN Secret so the NEXT run mints with the freshest token.
	broadPATBaoPath = "secret/linode/broad-pat"
	// broadPATSecretName is the GitHub Actions secret every workflow reads.
	broadPATSecretName = "LINODE_API_TOKEN"
)

// envSecretWriter publishes value into the infra-<deployment> GitHub environment
// secret `name` (seamed for tests; real = ghSetEnvSecretNative).
type envSecretWriter func(name, env, value string) error

var ghSetEnvSecretFn envSecretWriter = ghSetEnvSecretNative

// broadPATDeps are the injected collaborators (Linode API, OpenBao, GitHub writeback,
// clock) so the rotation flow is unit-testable without any network.
type broadPATDeps struct {
	lc          rotatorLinodeAPI
	bao         baoStore
	writeSecret envSecretWriter
	now         func() time.Time
}

// broadPATOpts are the run parameters (label, deployments, cadence, safety windows).
type broadPATOpts struct {
	label       string
	deployments []string // infra-<d> environments to publish LINODE_API_TOKEN into
	rotateAfter int      // days; rotate when the OpenBao rotated_at is older
	graceDays   int64    // don't revoke a sibling newer than this (CI may still use it)
	apply       bool
}

func ciRotateBroadPATCmd() *cobra.Command {
	var apply bool
	c := &cobra.Command{
		Use:   "rotate-broad-pat",
		Short: "rotate the broad Linode CI/TF PAT in-cluster: mint, seed OpenBao, publish to GitHub env secrets, revoke old",
		Long: "In-cluster rotator for the broad account:read_write Linode PAT (LINODE_API_TOKEN).\n" +
			"Runs in a dedicated CronJob (not the reconciler). When the OpenBao rotated_at is\n" +
			"older than --rotate-after-days: mints a fresh broad PAT with the current token,\n" +
			"verifies it, writes it to OpenBao, publishes it to each infra-<deployment> GitHub\n" +
			"environment secret (sealed box), then revokes older same-labeled PATs outside the\n" +
			"grace window. Dry-run unless --apply. Env: LINODE_TOKEN (current broad PAT),\n" +
			"BROAD_PAT_LABEL, BROAD_PAT_DEPLOYMENTS (space-separated), GH_TOKEN, GH_REPO,\n" +
			"ROTATE_AFTER_DAYS, GRACE_DAYS, OPENBAO_* (Kubernetes-auth: broad-pat-rotator role).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runRotateBroadPAT(context.Background(), apply) },
	}
	c.Flags().BoolVar(&apply, "apply", false, "actually rotate; without it, report whether a rotation is due and exit")
	return c
}

func runRotateBroadPAT(ctx context.Context, apply bool) error {
	minting := os.Getenv("LINODE_TOKEN")
	if minting == "" {
		return fmt.Errorf("LINODE_TOKEN must be set (the current broad PAT used to mint its successor)")
	}
	label := os.Getenv("BROAD_PAT_LABEL")
	if label == "" {
		return fmt.Errorf("BROAD_PAT_LABEL must be set (the Linode label of the broad PAT family, e.g. gha-<instance>_LINODE_API_TOKEN)")
	}
	deployments := strings.Fields(os.Getenv("BROAD_PAT_DEPLOYMENTS"))
	if apply && len(deployments) == 0 {
		return fmt.Errorf("BROAD_PAT_DEPLOYMENTS must list the infra-<deployment> environments to publish %s into", broadPATSecretName)
	}
	if apply {
		if os.Getenv("GH_TOKEN") == "" || os.Getenv("GH_REPO") == "" {
			return fmt.Errorf("GH_TOKEN and GH_REPO must be set to publish the rotated %s", broadPATSecretName)
		}
	}
	opts := broadPATOpts{
		label:       label,
		deployments: deployments,
		rotateAfter: int(cli.EnvInt("ROTATE_AFTER_DAYS", 60)),
		graceDays:   cli.EnvInt("GRACE_DAYS", 7),
		apply:       apply,
	}

	bao, err := newRotatorBaoStore(ctx)
	if err != nil {
		return err
	}
	deps := broadPATDeps{
		lc:          linodeRotatorClient(minting),
		bao:         bao,
		writeSecret: ghSetEnvSecretFn,
		now:         rotatorNow,
	}
	record, err := rotateBroadPAT(ctx, deps, opts)
	if err != nil {
		return err
	}
	return cli.PrintRecord(record)
}

// rotateBroadPAT is the pure-ish rotation flow (all I/O behind broadPATDeps). It
// returns a JSON audit record. Not-due and dry-run runs mint nothing and revoke
// nothing.
func rotateBroadPAT(ctx context.Context, d broadPATDeps, o broadPATOpts) (map[string]any, error) {
	now := d.now()
	rotatedAt, _, err := d.bao.Get(ctx, broadPATBaoPath, "rotated_at")
	if err != nil {
		return nil, fmt.Errorf("read %s rotated_at: %w", broadPATBaoPath, err)
	}
	record := map[string]any{
		"event":          "broad-pat-rotator",
		"timestamp_unix": now.Unix(),
		"label":          o.label,
		"dry_run":        !o.apply,
		"rotated_at":     rotatedAt,
	}
	if !isDue(rotatedAt, now, o.rotateAfter) {
		record["action"] = "skip"
		record["reason"] = fmt.Sprintf("not due (threshold %dd)", o.rotateAfter)
		return record, nil
	}
	if !o.apply {
		record["action"] = "would-rotate"
		return record, nil
	}

	// 1-2. Mint the successor with the current token, VERIFY before touching anything.
	expiry := linode.FmtLinodeTS(now.Unix() + broadPATValidityDays*linode.DaySecs)
	minted, err := d.lc.CreateProfileToken(ctx, o.label, broadPATScopes, expiry)
	if err != nil {
		return nil, fmt.Errorf("mint broad PAT %q: %w", o.label, err)
	}
	newID, _ := cli.AsUint64(minted["id"])
	newToken := cli.AsString(minted["token"])
	if newToken == "" {
		return nil, fmt.Errorf("mint broad PAT: response missing .token")
	}
	maskGHA(newToken)
	if err := linodeRotatorClient(newToken).Verify(ctx); err != nil {
		return nil, fmt.Errorf("verify freshly-minted broad PAT (id=%d): %w — nothing written or revoked", newID, err)
	}

	// 3. Write to OpenBao (the cluster's own copy; ESO refreshes LINODE_TOKEN for next run).
	if err := d.bao.Write(ctx, broadPATBaoPath, map[string]string{
		"token":      newToken,
		"rotated_at": strconv.FormatInt(now.Unix(), 10),
	}); err != nil {
		return nil, fmt.Errorf("write %s: %w — new PAT id=%d is live but not published; not revoking anything", broadPATBaoPath, err, newID)
	}

	// 4. Publish to every deployment's GitHub environment secret. A partial failure
	//    is safe (old token still valid until the grace-windowed revoke) but fatal to
	//    the run so revoke is skipped and it retries next cadence.
	published := []string{}
	for _, dep := range o.deployments {
		env := "infra-" + dep
		if err := d.writeSecret(broadPATSecretName, env, newToken); err != nil {
			record["action"] = "published-partial"
			record["new_pat_id"] = newID
			record["published_envs"] = published
			return record, fmt.Errorf("publish %s to %s: %w — old PAT NOT revoked (safe: still valid); retry next run", broadPATSecretName, env, err)
		}
		published = append(published, env)
	}

	// 5. Only now revoke older siblings, keeping the newest + anything within grace.
	revoked, skipped := revokeOldBroadPATs(ctx, d.lc, o.label, o.graceDays, now)

	record["action"] = "rotated"
	record["new_pat_id"] = newID
	record["published_envs"] = published
	record["revoked_ids"] = revoked
	record["skipped_in_grace_ids"] = skipped
	return record, nil
}

// revokeOldBroadPATs revokes same-labeled PATs older than the grace window, ALWAYS
// keeping the newest (the just-minted one). Best-effort: the new token is already
// live + published, so a failed list/revoke is logged and converges next run — it
// never fails the rotation. Mirrors the credentials-pat revoke-old grace logic.
func revokeOldBroadPATs(ctx context.Context, lc rotatorLinodeAPI, label string, graceDays int64, now time.Time) (revoked, skipped []uint64) {
	revoked, skipped = []uint64{}, []uint64{}
	items, err := lc.ListProfileTokens(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::broad-pat: list for drain failed (new PAT is live; converges next run): %v\n", err)
		return revoked, skipped
	}
	type cand struct {
		id      uint64
		created int64
	}
	var cands []cand
	for _, it := range items {
		if cli.AsString(it["label"]) != label {
			continue
		}
		id, ok := cli.AsUint64(it["id"])
		if !ok {
			continue
		}
		created, _ := linode.ParseTS(cli.AsString(it["created"]))
		cands = append(cands, cand{id, created})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].created > cands[j].created })
	if len(cands) == 0 {
		return revoked, skipped
	}
	cutoff := now.Unix() - graceDays*linode.DaySecs
	for _, c := range cands[1:] { // [0] is the newest (just minted) — never revoke it
		if c.created > cutoff {
			skipped = append(skipped, c.id) // still in grace — a consumer may hold it
			continue
		}
		if err := lc.DeleteProfileToken(ctx, c.id); err != nil {
			fmt.Fprintf(os.Stderr, "::warning::broad-pat: revoke id=%d failed (retries next run): %v\n", c.id, err)
			continue
		}
		revoked = append(revoked, c.id)
	}
	return revoked, skipped
}
