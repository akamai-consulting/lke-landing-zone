package credrotate

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
//   3. write it back to each infra-<deployment> GitHub ENVIRONMENT secret
//      (LINODE_API_TOKEN), sealed-box — the copies the workflows actually read
//   4. write it to OpenBao (secret/linode/broad-pat) so the cluster's own copy (and
//      the next run's LINODE_TOKEN, via ESO) update
//
//      GitHub BEFORE OpenBao, because the OpenBao write is what stamps rotated_at
//      and rotated_at is what `IsDue` reads. With the writes the other way round, a
//      single failed publish left a FRESH stamp behind: the next weekly run saw
//      "not due", returned action=skip with exit 0, and did so for the whole
//      60-day ROTATE_AFTER_DAYS window — while the 90-day PAT that GitHub still
//      held expired ~30 days into it. CI then broke with no failing job anywhere:
//      every run after the first exits 0, and the credential-age gauge reads the
//      OpenBao stamp the premature write had just refreshed. Reversed, a
//      PERSISTENT OpenBao failure leaves the stamp old, so
//      LLZCredentialRotationOverdue (broad-pat is reconcilelanes.CredClassAutomated) becomes
//      reachable. One new mode, stated for honesty: a persistent PUBLISH failure
//      now mints a PAT per weekly run without reaching revoke — self-limiting at
//      ~13 against the 100-PAT cap given the 90-day expiry. Reversed, a failed OpenBao write leaves the stamp OLD, so
//      the next run is still due, re-mints with the un-revoked old token, and
//      converges.
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
)

const (
	// BroadPATScopes MUST include account:read_write (to mint successor PATs) — the
	// scope the narrow in-cluster PAT deliberately withholds. Mirrors the scope set
	// the CI create-linode-pat job used.
	// MUST remain a superset of InClusterPATScopes: the rotated broad PAT becomes
	// each deployment's LINODE_API_TOKEN, which mint-bootstrap-pat then uses to mint
	// the narrow in-cluster PAT — and Linode rejects creating a token with scopes
	// GREATER than the requesting token's. `domains:read_write` is here for exactly
	// that reason (the in-cluster PAT carries it); dropping it 400s the next bootstrap
	// after a rotation. TestBroadPATScopesSupersetInclusterPAT guards the invariant.
	BroadPATScopes       = "domains:read_write linodes:read_write object_storage:read_write lke:read_write firewall:read_write vpc:read_write volumes:read_write nodebalancers:read_write events:read_only account:read_write"
	BroadPATValidityDays = 90
	// BroadPATBaoPath is the cluster's own copy of the broad token; ESO syncs it to
	// the rotator's LINODE_TOKEN Secret so the NEXT run mints with the freshest token.
	BroadPATBaoPath = "secret/linode/broad-pat"
	// broadPATSecretName is the GitHub Actions secret every workflow reads.
	broadPATSecretName = "LINODE_API_TOKEN"
)

// envSecretWriter publishes value into the infra-<deployment> GitHub environment
// secret `name` (seamed for tests; real = ghSetEnvSecretNative).
type envSecretWriter func(name, env, value string) error

// ghSetEnvSecretFn defaults to this package's own SetSecret seam.
//
// It used to point at package main's ghSetEnvSecretNative directly. Both are the
// same capability — write a GitHub secret, scoped to an environment — and this
// package already had a seam for it, installed by cmd/llz. Keeping two would mean
// two things to stub and one of them silently reaching a live forge.
var ghSetEnvSecretFn envSecretWriter = func(name, env, value string) error { return SetSecret(name, env, value) }

// broadPATDeps are the injected collaborators (Linode API, OpenBao, GitHub writeback,
// clock) so the rotation flow is unit-testable without any network.
type broadPATDeps struct {
	lc          LinodeAPI
	bao         BaoStore
	writeSecret envSecretWriter
	now         func() time.Time
}

// BroadPATOpts are the run parameters (label, deployments, cadence, safety windows).
type BroadPATOpts struct {
	label       string
	deployments []string // infra-<d> environments to publish LINODE_API_TOKEN into
	rotateAfter int      // days; rotate when the OpenBao rotated_at is older
	graceDays   int64    // don't revoke a sibling newer than this (CI may still use it)
	apply       bool
}

func RunRotateBroadPAT(ctx context.Context, apply bool) error {
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
	opts := BroadPATOpts{
		label:       label,
		deployments: deployments,
		rotateAfter: int(cli.EnvInt("ROTATE_AFTER_DAYS", 60)),
		graceDays:   cli.EnvInt("GRACE_DAYS", 7),
		apply:       apply,
	}

	bao, err := NewBaoStore(ctx)
	if err != nil {
		return err
	}
	deps := broadPATDeps{
		lc:          NewLinodeClient(minting),
		bao:         bao,
		writeSecret: ghSetEnvSecretFn,
		now:         Now,
	}
	record, err := RotateBroadPAT(ctx, deps, opts)
	if err != nil {
		return err
	}
	return cli.PrintRecord(record)
}

// RotateBroadPAT is the pure-ish rotation flow (all I/O behind broadPATDeps). It
// returns a JSON audit record. Not-due and dry-run runs mint nothing and revoke
// nothing.
func RotateBroadPAT(ctx context.Context, d broadPATDeps, o BroadPATOpts) (map[string]any, error) {
	now := d.now()
	rotatedAt, _, err := d.bao.Get(ctx, BroadPATBaoPath, "rotated_at")
	if err != nil {
		return nil, fmt.Errorf("read %s rotated_at: %w", BroadPATBaoPath, err)
	}
	record := map[string]any{
		"event":          "broad-pat-rotator",
		"timestamp_unix": now.Unix(),
		"label":          o.label,
		"dry_run":        !o.apply,
		"rotated_at":     rotatedAt,
	}
	if !IsDue(rotatedAt, now, o.rotateAfter) {
		record["action"] = "skip"
		record["reason"] = fmt.Sprintf("not due (threshold %dd)", o.rotateAfter)
		return record, nil
	}
	if !o.apply {
		record["action"] = "would-rotate"
		return record, nil
	}

	// 1-2. Mint the successor with the current token, VERIFY before touching anything.
	expiry := linode.FmtLinodeTS(now.Unix() + BroadPATValidityDays*linode.DaySecs)
	minted, err := d.lc.CreateProfileToken(ctx, o.label, BroadPATScopes, expiry)
	if err != nil {
		return nil, fmt.Errorf("mint broad PAT %q: %w", o.label, err)
	}
	newID, _ := cli.AsUint64(minted["id"])
	newToken := cli.AsString(minted["token"])
	if newToken == "" {
		return nil, fmt.Errorf("mint broad PAT: response missing .token")
	}
	maskGHA(newToken)
	if err := NewLinodeClient(newToken).Verify(ctx); err != nil {
		return nil, fmt.Errorf("verify freshly-minted broad PAT (id=%d): %w — nothing written or revoked", newID, err)
	}

	// 3. Publish to every deployment's GitHub environment secret. A partial failure
	//    is safe (old token still valid until the grace-windowed revoke) and fatal to
	//    the run so revoke is skipped — and because rotated_at has NOT been stamped
	//    yet, the next run is still due and actually retries.
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

	// 4. Write to OpenBao (the cluster's own copy; ESO refreshes LINODE_TOKEN for next
	//    run). This is the step that stamps rotated_at, so it goes AFTER the publish —
	//    see the FLOW note above.
	if err := d.bao.Write(ctx, BroadPATBaoPath, map[string]string{
		"token":      newToken,
		"rotated_at": strconv.FormatInt(now.Unix(), 10),
	}); err != nil {
		return nil, fmt.Errorf("write %s: %w — new PAT id=%d is published to GitHub but the cluster copy is stale; "+
			"not revoking anything, and rotated_at is unstamped so the next run retries", BroadPATBaoPath, err, newID)
	}

	// 5. Only now revoke older siblings, keeping the newest + anything within grace.
	revoked, skipped := RevokeOldBroadPATs(ctx, d.lc, o.label, o.graceDays, now)

	record["action"] = "rotated"
	record["new_pat_id"] = newID
	record["published_envs"] = published
	record["revoked_ids"] = revoked
	record["skipped_in_grace_ids"] = skipped
	return record, nil
}

// RevokeOldBroadPATs revokes same-labeled PATs older than the grace window, ALWAYS
// keeping the newest (the just-minted one). Best-effort: the new token is already
// live + published, so a failed list/revoke is logged and converges next run — it
// never fails the rotation. Mirrors the credentials-pat revoke-old grace logic.
func RevokeOldBroadPATs(ctx context.Context, lc LinodeAPI, label string, graceDays int64, now time.Time) (revoked, skipped []uint64) {
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
