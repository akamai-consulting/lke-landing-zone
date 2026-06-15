package main

// ci_gh_pat_expiry.go implements `llz ci gh-pat-expiry` — the native port of the
// scheduled-checks `gh-pat-expiry-health` job. GitHub exposes no classic-PAT list
// API, so each known service PAT is probed individually and its
// `GitHub-Authentication-Token-Expiration` response header inspected: a missing
// header means a never-expiring classic PAT (the core token-lifetime policy
// violation); a 401/403 or already-expired/over-policy token also fails the job.
// Unreachable / not-set / unparseable are warn-only.
//
// The classification ladder is the unit-tested internal/health logic; this file
// is the one-request-per-token HTTP probe plus the $GITHUB_STEP_SUMMARY table.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/spf13/cobra"
)

// ghPATProbe performs one authenticated request and returns the HTTP status
// (0 == unreachable) and the raw token-expiration header. Package var so the
// command is exercisable without network access.
var ghPATProbe = func(api, token string) (code int, expHeader string, err error) {
	url := strings.TrimRight(api, "/") + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err // unreachable — code 0
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("GitHub-Authentication-Token-Expiration"), nil
}

// patTarget is one service PAT to self-check: its display name, the API base to
// probe, and the token value (empty when the secret isn't set).
type patTarget struct {
	name  string
	api   string
	token string
}

func ciGHPATExpiryCmd() *cobra.Command {
	var maxDays, warnDays int
	c := &cobra.Command{
		Use:   "gh-pat-expiry",
		Short: "self-check GitHub service-PAT expiry via the token-expiration header",
		Long: "Native port of the gh-pat-expiry-health scheduled job. Probes each configured\n" +
			"github.com service PAT and reads its GitHub-Authentication-Token-Expiration\n" +
			"header, failing the job on a missing-expiry (never-expiring classic PAT), a\n" +
			"401/403, an already-expired token, or a lifetime over --max-days. Tokens come\n" +
			"from the environment (OPENBAO_SECRETS_WRITE_TOKEN, APL_VALUES_REPO_TOKEN); the\n" +
			"API base is $GITHUB_API (default https://api.github.com).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIGHPATExpiry(maxDays, warnDays) },
	}
	f := c.Flags()
	f.IntVar(&maxDays, "max-days", 90, "fail a token whose lifetime exceeds this many days")
	f.IntVar(&warnDays, "warn-days", 14, "warn when a token expires within this many days")
	return c
}

func runCIGHPATExpiry(maxDays, warnDays int) error {
	apiBase := envOr("GITHUB_API", "https://api.github.com")
	targets := []patTarget{
		{"OPENBAO_SECRETS_WRITE_TOKEN", apiBase, os.Getenv("OPENBAO_SECRETS_WRITE_TOKEN")},
		{"APL_VALUES_REPO_TOKEN", apiBase, os.Getenv("APL_VALUES_REPO_TOKEN")},
	}

	summary := []string{
		fmt.Sprintf("## GitHub Service PAT Expiry — %s", time.Now().UTC().Format("2006-01-02T15:04Z")),
		"",
		"| Token | Host | Result |",
		"|-------|------|--------|",
	}

	now := time.Now()
	failed := 0
	for _, tgt := range targets {
		present := tgt.token != ""
		code, expHeader := 0, ""
		if present {
			fmt.Fprintf(os.Stderr, "::add-mask::%s\n", tgt.token)
			var err error
			code, expHeader, err = ghPATProbe(tgt.api, tgt.token)
			if err != nil {
				code = 0 // unreachable
			}
		}
		state, days := health.ClassifyPATResponse(present, code, expHeader, now, maxDays, warnDays)
		annotation, cell := patReport(tgt, state, days, code, maxDays, warnDays)
		if annotation != "" {
			fmt.Fprintln(os.Stderr, annotation)
		}
		summary = append(summary, fmt.Sprintf("| %s | %s | %s |", tgt.name, tgt.api, cell))
		if state.Category() == health.CatFail {
			failed++
		}
	}

	summary = append(summary,
		"",
		"> Per-token header self-check (GitHub has no classic-PAT list API).",
		"> Ad-hoc individual PATs created by humans are not covered here.",
		"> See docs/runbooks/linode-credential-rotation.md.")
	if err := appendGHAFile("GITHUB_STEP_SUMMARY", summary...); err != nil {
		return err
	}

	if failed > 0 {
		return fmt.Errorf("%d GitHub service PAT(s) violate the has-expiry / ≤%d-day policy", failed, maxDays)
	}
	fmt.Printf("All checked GitHub service PATs have an expiry within the ≤%d-day policy.\n", maxDays)
	return nil
}

// patReport returns the ::warning::/::error:: annotation (empty for OK) and the
// step-summary result cell for one token's verdict — the presentation half of
// the job's check(), kept out of internal/health (which decides, not formats).
func patReport(tgt patTarget, state health.PATCheckState, days, code, maxDays, warnDays int) (annotation, cell string) {
	switch state {
	case health.PATNotSet:
		return fmt.Sprintf("::warning::%s not set — cannot verify its expiry (expected service PAT missing?)", tgt.name), "⚠️ not set"
	case health.PATUnreachable:
		return fmt.Sprintf("::warning::%s: %s unreachable from the runner — cannot verify (not failing the job on connectivity)", tgt.name, tgt.api), "⚠️ unreachable"
	case health.PATInvalid:
		return fmt.Sprintf("::error::%s: %s returned %d — token invalid, revoked, or expired. Rotate it.", tgt.name, tgt.api, code),
			fmt.Sprintf("❌ HTTP %d (invalid/expired)", code)
	case health.PATNoExpiry:
		return fmt.Sprintf("::error::%s: no GitHub-Authentication-Token-Expiration header — token has NO expiry (never-expiring classic PAT). Recreate with a ≤%d-day expiry.", tgt.name, maxDays),
			"❌ no expiry set"
	case health.PATUnparseable:
		return fmt.Sprintf("::warning::%s: could not parse expiry — verify manually", tgt.name), "⚠️ unparseable expiry"
	case health.PATExpired:
		return fmt.Sprintf("::error::%s: expired. Rotate it now.", tgt.name), "❌ expired"
	case health.PATOverPolicy:
		return fmt.Sprintf("::error::%s: expires in %dd (>%dd) — lifetime exceeds the 90-day token policy. Recreate with a ≤%d-day expiry.", tgt.name, days, maxDays, maxDays),
			fmt.Sprintf("❌ %dd left (>%dd policy)", days, maxDays)
	case health.PATWarn:
		return fmt.Sprintf("::warning::%s: expires in %dd (≤%dd) — rotate it soon", tgt.name, days, warnDays),
			fmt.Sprintf("⚠️ %dd left", days)
	default: // PATOK
		return "", fmt.Sprintf("✅ %dd left", days)
	}
}

// envOr returns the environment value for key, or def when unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
