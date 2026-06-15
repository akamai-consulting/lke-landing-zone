package main

// ci_cred_audit.go implements `llz ci cred-audit` — the former standalone
// linode-cred-audit binary plus the bash wrapper llz-scheduled-checks.yml kept
// around it (the not-bootstrapped skip, the exit-code bucketing, the step
// summary), folded into llz like the credential rotators.
//
// PAT-rotation policy backstop. The cloud secrets rotation guidelines
// require User/Service API keys & PATs to expire within 90 days (this is the
// concrete, scriptable check for it), and Object Storage bucket access keys
// revoked after 120 days. Read-only:
//  1. Lists every PAT on the account profile and asserts each has a non-null
//     expiry whose lifetime is ≤ --max-pat-days, is not already expired, and
//     is not within --warn-days of expiring.
//  2. Inventories every Object Storage key. The Linode API exposes no creation
//     timestamp for OBJ keys, so the 120-day age cannot be derived here — that
//     SLA is enforced by the loki-objkey-rotation-health check (OpenBao secret
//     version age) and the declarative time_rotating trigger in the
//     object-storage Terraform module. Enumerate only.
//
// Prints one JSON audit record on stdout (the SLA evidence), appends it to the
// step summary, and fails (exit 1) on any policy violation — or, with
// --strict, on a near-expiry warning.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	"github.com/spf13/cobra"
)

// credLister is the read-only slice of the Linode client the audit needs.
// Injecting it lets the PAT expiry policy logic run against canned responses.
type credLister interface {
	ListProfileTokens(ctx context.Context) ([]map[string]any, error)
	ListObjectStorageKeys(ctx context.Context) ([]map[string]any, error)
}

var newCredAuditClient = func(token string) credLister { return linode.NewClient(token, 30*time.Second) }

type credAuditOpts struct {
	maxPATDays int64
	warnDays   int64
	strict     bool
}

func ciCredAuditCmd() *cobra.Command {
	var o credAuditOpts
	c := &cobra.Command{
		Use:   "cred-audit",
		Short: "audit Linode PAT expiry (90-day policy) + inventory Object Storage keys",
		Long: "The former standalone linode-cred-audit binary plus its scheduled-checks\n" +
			"wrapper. Asserts every account PAT has a non-null expiry, a lifetime within\n" +
			"--max-pat-days, and is neither expired nor (with --strict, fatally) near\n" +
			"expiry; inventories OBJ keys (their 120-day SLA is enforced by\n" +
			"health-loki-objkey-rotation + the Terraform time_rotating trigger — Linode\n" +
			"exposes no OBJ-key creation time). Prints one JSON audit record (the SLA\n" +
			"evidence) and writes the step summary. A deployment without\n" +
			"LINODE_TOKEN/LINODE_API_TOKEN set skips cleanly (env not bootstrapped).\n" +
			"Reads REGION for the report headings.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
			return runCICredAudit(o)
		},
	}
	f := c.Flags()
	f.Int64Var(&o.maxPATDays, "max-pat-days", cli.EnvInt("MAX_PAT_DAYS", 90), "maximum allowed PAT lifetime in days")
	f.Int64Var(&o.warnDays, "warn-days", cli.EnvInt("WARN_DAYS", 14), "warn when a PAT expires within this many days")
	f.BoolVar(&o.strict, "strict", cli.EnvBool("AUDIT_STRICT", false), "treat near-expiry warnings as failures")
	return c
}

func runCICredAudit(o credAuditOpts) error {
	region := os.Getenv("REGION")
	if err := appendGHAFile("GITHUB_STEP_SUMMARY",
		fmt.Sprintf("## Linode PAT Expiry — %s — %s", region, time.Now().UTC().Format("2006-01-02T15:04Z")),
		""); err != nil {
		return err
	}

	// Self-skip a deployment that has no Linode token in its infra-<env>
	// Environment yet (not bootstrapped — mirrors the allow-missing skip the
	// cluster-access jobs use). Without this, a newly-scaffolded deployment in
	// the discovered matrix would go red purely for lacking a secret.
	token := firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("LINODE_API_TOKEN"))
	if token == "" {
		fmt.Fprintf(os.Stderr, "::warning::LINODE_API_TOKEN not set for infra-%s — skipping PAT audit (env not bootstrapped). Seed the secret to enable it.\n", region)
		return appendGHAFile("GITHUB_STEP_SUMMARY",
			fmt.Sprintf("> Skipped: `LINODE_API_TOKEN` not set in `infra-%s` (env not bootstrapped).", region))
	}

	record, violated, err := credAudit(context.Background(), newCredAuditClient(token), o)
	if err != nil {
		return fmt.Errorf("cred-audit failed to run on %s (%v) — check the Linode API / token scope", region, err)
	}
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := cli.PrintRecord(record); err != nil {
		return err
	}
	if err := appendGHAFile("GITHUB_STEP_SUMMARY", "```json", string(recordJSON), "```"); err != nil {
		return err
	}

	if violated {
		fmt.Fprintf(os.Stderr, "::error::Linode PAT policy breach on %s — a token has no expiry, a >%d-day lifetime, or is expired. See docs/runbooks/linode-credential-rotation.md.\n", region, o.maxPATDays)
		if err := appendGHAFile("GITHUB_STEP_SUMMARY",
			fmt.Sprintf("> **CRITICAL:** PAT policy breach. Rotate the offending token in Cloud Manager with a ≤%d-day expiry (docs/runbooks/linode-credential-rotation.md).", o.maxPATDays)); err != nil {
			return err
		}
		return fmt.Errorf("linode PAT policy breach on %s", region)
	}
	fmt.Printf("All Linode PATs on %s satisfy the ≤%d-day policy.\n", region, o.maxPATDays)
	return appendGHAFile("GITHUB_STEP_SUMMARY",
		fmt.Sprintf("> All PATs within the ≤%d-day policy.", o.maxPATDays))
}

// credAudit runs the policy checks and builds the audit record. violated
// reports whether the run must fail (a violation, or a warning under strict).
func credAudit(ctx context.Context, client credLister, o credAuditOpts) (record map[string]any, violated bool, err error) {
	now := time.Now().Unix()
	maxLife := o.maxPATDays * linode.DaySecs
	warnWindow := o.warnDays * linode.DaySecs

	// ── 1. PAT expiry policy ──
	tokens, err := client.ListProfileTokens(ctx)
	if err != nil {
		return nil, false, err
	}
	violations := []any{}
	warnings := []any{}
	for _, t := range tokens {
		label := unlabelled(linode.MapString(t, "label"))
		id := linode.MapUint(t, "id")
		created, hasCreated := linode.ParseTS(linode.MapString(t, "created"))
		expiry, hasExpiry := linode.ParseTS(linode.MapString(t, "expiry"))

		switch {
		case !hasExpiry:
			slog.Error("PAT has no expiry — violates the 90-day policy", "token", label, "id", id)
			violations = append(violations, map[string]any{"id": id, "label": label, "reason": "no_expiry"})
		case expiry <= now:
			slog.Error("PAT is expired", "token", label, "id", id)
			violations = append(violations, map[string]any{"id": id, "label": label, "reason": "expired", "expiry_unix": expiry})
		case hasCreated && expiry-created > maxLife:
			days := (expiry - created) / linode.DaySecs
			slog.Error("PAT lifetime exceeds the 90-day policy", "token", label, "id", id, "lifetime_days", days)
			violations = append(violations, map[string]any{
				"id": id, "label": label, "reason": "lifetime_exceeds_max",
				"lifetime_days": days, "max_pat_days": o.maxPATDays,
			})
		case expiry-now <= warnWindow:
			days := (expiry - now) / linode.DaySecs
			slog.Warn("PAT expires soon — rotate it", "token", label, "id", id, "days_left", days)
			warnings = append(warnings, map[string]any{"id": id, "label": label, "reason": "near_expiry", "days_left": days})
		default:
			slog.Info("PAT OK", "token", label, "id", id, "days_left", (expiry-now)/linode.DaySecs)
		}
	}

	// ── 2. Object Storage key inventory (no API-side age; enumerate only) ──
	objKeys, err := client.ListObjectStorageKeys(ctx)
	if err != nil {
		return nil, false, err
	}
	objInventory := make([]any, 0, len(objKeys))
	for _, k := range objKeys {
		label := unlabelled(linode.MapString(k, "label"))
		buckets := 0
		if ba, ok := k["bucket_access"].([]any); ok {
			buckets = len(ba)
		}
		objInventory = append(objInventory, map[string]any{
			"id":                  k["id"],
			"label":               label,
			"limited":             k["limited"],
			"bucket_access_count": buckets,
			"is_loki_key":         strings.HasPrefix(label, "loki-"),
		})
	}
	slog.Info("Object Storage keys inventoried — 120-day age enforced by loki-objkey-rotation-health "+
		"+ the Terraform time_rotating trigger (Linode exposes no OBJ-key creation time)",
		"obj_key_count", len(objInventory))

	// ── Audit record (single structured line — SLA evidence) ──
	violated = len(violations) > 0 || (o.strict && len(warnings) > 0)
	result := "PASS"
	if violated {
		result = "FAIL"
	} else if len(warnings) > 0 {
		result = "PASS_WITH_WARNINGS"
	}
	return map[string]any{
		"event":          "linode-cred-audit",
		"timestamp_unix": now,
		"pat_count":      len(tokens),
		"max_pat_days":   o.maxPATDays,
		"warn_days":      o.warnDays,
		"strict":         o.strict,
		"pat_violations": violations,
		"pat_warnings":   warnings,
		"obj_keys":       objInventory,
		"result":         result,
	}, violated, nil
}

// unlabelled substitutes a placeholder for an empty or missing API label, so
// the audit record never reports a blank token/key name.
func unlabelled(label string) string {
	if label == "" {
		return "<unlabelled>"
	}
	return label
}
