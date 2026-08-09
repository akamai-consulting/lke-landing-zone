package credrotate

// credentials_pat.go implements `llz credentials pat create|revoke-old` — the
// LINODE_API_TOKEN (shared Linode PAT) lifecycle, moved verbatim from the
// former cmd/linode-pat-rotator binary. See credentials.go for the shared
// contract (one JSON record on stdout, logs + ::add-mask:: on stderr, dry-run
// unless armed).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

func RunPATCreate(ctx context.Context, client PATAPI, apply bool, label, scopes string, validityDays int64, ghaSecretName string, ghaDeployments []string) error {
	if validityDays > 90 {
		return fmt.Errorf("validity_days=%d exceeds the 90-day policy ceiling — refusing to create", validityDays)
	}
	if validityDays < 1 {
		return fmt.Errorf("validity_days=%d is non-positive", validityDays)
	}

	expiryUnix := time.Now().Unix() + validityDays*linode.DaySecs
	expiryTS := linode.FmtLinodeTS(expiryUnix)
	slog.Info("creating PAT", "label", label, "scopes", scopes, "validity_days", validityDays, "expiry", expiryTS)

	if !apply {
		slog.Warn("DRY-RUN: would POST /v4/profile/tokens")
		return cli.PrintRecord(map[string]any{
			"event":          "linode-pat-rotator.create",
			"timestamp_unix": time.Now().Unix(),
			"dry_run":        true,
			"label":          label,
			"scopes":         scopes,
			"validity_days":  validityDays,
			"expiry_planned": expiryTS,
		})
	}

	resp, err := client.CreateProfileToken(ctx, label, scopes, expiryTS)
	if err != nil {
		return err
	}
	newID, ok := cli.AsUint64(resp["id"])
	if !ok {
		return fmt.Errorf("create response missing .id")
	}
	newToken, ok := resp["token"].(string)
	if !ok || newToken == "" {
		return fmt.Errorf("create response missing .token")
	}
	// The token leaks if a caller forgets to mask it. Emit a GH-Actions
	// ::add-mask:: on stderr so a step that pipes our stdout through `tee` is
	// still scrubbed in the surrounding logs.
	fmt.Fprintf(os.Stderr, "::add-mask::%s\n", newToken)

	// Persist the new token into the GitHub secret for each infra-<deployment>
	// environment — the env-scoped copies the workflows actually read (a repo-level
	// write would be shadowed by the per-env secret and never picked up).
	if ghaSecretName != "" {
		if err := WriteRotatedSecret(ghaSecretName, newToken, ghaDeployments); err != nil {
			return err
		}
		slog.Info("updated GHA secret", "name", ghaSecretName, "deployments", ghaDeployments)
	}

	slog.Info("created new PAT", "new_pat_id", newID)
	return cli.PrintRecord(map[string]any{
		"event":          "linode-pat-rotator.create",
		"timestamp_unix": time.Now().Unix(),
		"dry_run":        false,
		"label":          label,
		"scopes":         scopes,
		"validity_days":  validityDays,
		"new_pat_id":     newID,
		"new_token":      newToken,
		"expiry":         resp["expiry"],
	})
}

func RunPATRevokeOld(ctx context.Context, client PATAPI, apply bool, label string, graceDays int64) error {
	if graceDays < 0 {
		return fmt.Errorf("grace_days=%d must be non-negative", graceDays)
	}

	now := time.Now().Unix()
	cutoff := now - graceDays*linode.DaySecs
	tokens, err := client.ListProfileTokens(ctx)
	if err != nil {
		return err
	}

	// Candidates: every PAT whose label matches exactly, newest first.
	type cand struct {
		id      uint64
		created int64
	}
	var candidates []cand
	for _, t := range tokens {
		if s, _ := t["label"].(string); s != label {
			continue
		}
		id, ok := cli.AsUint64(t["id"])
		if !ok {
			continue
		}
		created, ok := linode.ParseTS(cli.AsString(t["created"]))
		if !ok {
			continue
		}
		candidates = append(candidates, cand{id, created})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].created > candidates[j].created })

	if len(candidates) == 0 {
		slog.Warn("no PATs match label — nothing to revoke", "label", label)
		return cli.PrintRecord(map[string]any{
			"event":                "linode-pat-rotator.revoke-old",
			"timestamp_unix":       now,
			"dry_run":              !apply,
			"label":                label,
			"grace_days":           graceDays,
			"kept_pat_id":          nil,
			"revoked_ids":          []uint64{},
			"skipped_in_grace_ids": []uint64{},
		})
	}

	keptID := candidates[0].id
	revoked := []uint64{}
	skipped := []uint64{}
	// Skip [0] (the live one); evaluate every older sibling.
	for _, c := range candidates[1:] {
		if c.created > cutoff {
			slog.Info("in grace window — keeping for now", "id", c.id, "age_days", (now-c.created)/linode.DaySecs)
			skipped = append(skipped, c.id)
			continue
		}
		if !apply {
			slog.Warn("DRY-RUN: would DELETE PAT", "id", c.id)
		} else {
			if err := client.DeleteProfileToken(ctx, c.id); err != nil {
				return err
			}
			slog.Info("revoked", "id", c.id)
		}
		revoked = append(revoked, c.id)
	}

	return cli.PrintRecord(map[string]any{
		"event":                "linode-pat-rotator.revoke-old",
		"timestamp_unix":       now,
		"dry_run":              !apply,
		"label":                label,
		"grace_days":           graceDays,
		"kept_pat_id":          keptID,
		"revoked_ids":          revoked,
		"skipped_in_grace_ids": skipped,
	})
}
