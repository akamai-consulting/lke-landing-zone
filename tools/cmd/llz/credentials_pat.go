package main

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/spf13/cobra"
)

func credentialsPATCmd(o *rotatorOpts) *cobra.Command {
	c := &cobra.Command{
		Use:   "pat",
		Short: "rotate the shared Linode PAT (90-day policy): create + revoke-old",
	}
	c.AddCommand(credentialsPATCreateCmd(o), credentialsPATRevokeOldCmd(o))
	return c
}

func credentialsPATCreateCmd(o *rotatorOpts) *cobra.Command {
	var label, scopes string
	var validityDays int64
	c := &cobra.Command{
		Use:   "create",
		Short: "mint a new PAT with the configured label/scopes/validity (JSON record on stdout)",
		Long: "Issues a new Linode PAT with the configured label, scopes, and validity,\n" +
			"printing the new id + token as one JSON record on stdout for the calling\n" +
			"composite action to propagate (GHA repo secret + each region's OpenBao).\n" +
			"Refuses validity-days > 90 (the 90-day policy ceiling). Dry-run unless --apply.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			token, apply, err := o.resolve()
			if err != nil {
				return err
			}
			return runCredentialsPATCreate(context.Background(), newPATRotatorClient(token), apply, label, scopes, validityDays)
		},
	}
	f := c.Flags()
	f.StringVar(&label, "label", os.Getenv("PAT_LABEL"), "label for the new PAT — also the revoke-old drain target (env PAT_LABEL)")
	f.StringVar(&scopes, "scopes", os.Getenv("PAT_SCOPES"), "Linode-API scopes string for the new PAT (env PAT_SCOPES)")
	f.Int64Var(&validityDays, "validity-days", cli.EnvInt("PAT_VALIDITY_DAYS", 90), "validity window in days; the 90-day policy caps this at 90 (env PAT_VALIDITY_DAYS)")
	return c
}

func credentialsPATRevokeOldCmd(o *rotatorOpts) *cobra.Command {
	var label string
	var graceDays int64
	c := &cobra.Command{
		Use:   "revoke-old",
		Short: "daily reaper: keep the newest same-labeled PAT, revoke older siblings past the grace window",
		Long: "Lists every PAT matching the label, keeps the newest (the live one), and\n" +
			"revokes any older sibling whose `created` time is past the grace window.\n" +
			"Stateless: the label IS the record of which PAT is current. Dry-run unless\n" +
			"--apply.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			token, apply, err := o.resolve()
			if err != nil {
				return err
			}
			return runCredentialsPATRevokeOld(context.Background(), newPATRotatorClient(token), apply, label, graceDays)
		},
	}
	f := c.Flags()
	f.StringVar(&label, "label", os.Getenv("PAT_LABEL"), "label to drain — same label `pat create` uses (env PAT_LABEL)")
	f.Int64Var(&graceDays, "grace-days", cli.EnvInt("PAT_GRACE_DAYS", 7), "only revoke same-labeled siblings older than this many days (env PAT_GRACE_DAYS)")
	return c
}

func runCredentialsPATCreate(ctx context.Context, client patAPI, apply bool, label, scopes string, validityDays int64) error {
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

func runCredentialsPATRevokeOld(ctx context.Context, client patAPI, apply bool, label string, graceDays int64) error {
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
