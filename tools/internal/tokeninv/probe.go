package tokeninv

// token_validate.go — active VALIDITY probing for the credentials `llz tokens`
// gathers, layered onto the presence-only readiness table (state.go). Presence
// ("is it set?") never catches the failure mode that actually bites: a token that
// IS set but is expired / revoked / mistyped, which sails through setup and then
// 401/403s deep inside a CI run (e.g. a GHCR token endpoint 403 mid-bootstrap).
// This makes one lightweight authenticated request per credential whose VALUE is
// available locally (freshly gathered or cached in .llz) and reports valid /
// INVALID / unreachable, so a dead token is caught at `llz tokens` / `llz doctor`
// time with a "rotate it" instead of a mid-provision mystery.
//
// GitHub exposes only secret NAMES, not values, so a secret configured ONLY on
// the repo (never gathered locally) cannot be probed here — those are reported as
// "set on GitHub (value not readable locally)". The in-CI counterpart, where the
// secret IS in the environment, is `llz ci gh-pat-expiry` (GitHub PATs).

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/color"
)

type ValidityStatus int

const (
	VSkipped     ValidityStatus = iota // no locally-readable value → not probed
	VValid                             // authenticated cleanly
	VWarn                              // authenticated but flagged (near expiry / policy)
	VInvalid                           // auth rejected (401/403), expired, or revoked
	VUnreachable                       // endpoint unreachable → couldn't verify
)

// tokenValidity is one credential's probe outcome.
type TokenValidity struct {
	Name   string
	Status ValidityStatus
	Detail string
}

// ── HTTP probe seams ─────────────────────────────────────────────────────────
//
// EXPORTED package vars. They were unexported when this lived in package main,
// where "package var so tests run without network" was enough. It no longer is:
// `llz doctor` renders its readiness table from ProbeToken, and its tests live in
// package main, so the seam has to be reachable from there or the doctor's tests
// would make real HTTP calls. Exported seams are a cost — anything can swap them —
// but a test suite that quietly dials the internet is a worse one.

// LinodeProbe GETs the Linode profile with the PAT as a Bearer token; the status
// (0 = unreachable) tells valid (2xx) from rejected (401/403). Any valid Linode
// token can read /v4/profile, so this confirms the token authenticates.
var LinodeProbe = func(token string) (int, error) {
	return bearerProbe("https://api.linode.com/v4/profile", token)
}

// GHCRTokenProbe hits the GHCR token endpoint with Basic auth (user:token) for a
// pull scope — the exact request `ghcrChartPublished` makes, so a 403 here is the
// same 403 that fails the chart pre-flight. Status 0 = unreachable.
var GHCRTokenProbe = func(user, token string) (int, error) {
	// Scope repo is immaterial to whether the CREDENTIAL authenticates; use a
	// stable first-party path. Username is ignored by GHCR's token endpoint but
	// must be non-empty for Basic auth.
	url := "https://ghcr.io/token?service=ghcr.io&scope=repository:x/x:pull"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if user == "" {
		user = "x"
	}
	req.SetBasicAuth(user, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func bearerProbe(url, token string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// ── pure classifiers (unit-tested) ────────────────────────────────────────────

// classifyAuthStatus maps an HTTP status from an authenticated probe to a
// validity verdict. 0 = unreachable (couldn't verify, not the token's fault);
// 2xx = valid; 401/403 = the token itself is bad; anything else = can't tell.
func classifyAuthStatus(code int) (ValidityStatus, string) {
	switch {
	case code == 0:
		return VUnreachable, "endpoint unreachable — could not verify (not failing on connectivity)"
	case code/100 == 2:
		return VValid, ""
	case code == 401 || code == 403:
		return VInvalid, fmt.Sprintf("auth rejected (HTTP %d) — token invalid, revoked, or expired; rotate it", code)
	default:
		return VUnreachable, fmt.Sprintf("unexpected HTTP %d — could not verify", code)
	}
}

// githubPATValidity turns the shared PAT classifier's verdict into a
// tokenValidity. Authenticating-but-flagged states (near expiry, over the 90-day
// policy, never-expiring) are VWarn — the token WORKS, but should be rotated.
func githubPATValidity(name string, state health.PATCheckState, days, code int) TokenValidity {
	switch state {
	case health.PATInvalid:
		return TokenValidity{name, VInvalid, fmt.Sprintf("GitHub returned %d — token invalid, revoked, or expired; rotate it", code)}
	case health.PATExpired:
		return TokenValidity{name, VInvalid, "expired — rotate it now"}
	case health.PATUnreachable:
		return TokenValidity{name, VUnreachable, "api.github.com unreachable — could not verify"}
	case health.PATNoExpiry:
		return TokenValidity{name, VWarn, "valid, but a never-expiring classic PAT (recreate with a ≤90-day expiry)"}
	case health.PATOverPolicy:
		return TokenValidity{name, VWarn, fmt.Sprintf("valid, but expires in %dd (>90-day policy)", days)}
	case health.PATWarn:
		return TokenValidity{name, VWarn, fmt.Sprintf("valid, expires in %dd — rotate soon", days)}
	case health.PATUnparseable:
		return TokenValidity{name, VWarn, "valid, but expiry header unparseable — verify manually"}
	default: // PATOK
		return TokenValidity{name, VValid, fmt.Sprintf("valid, expires in %dd", days)}
	}
}

// TokenKind selects the probe for a credential name. Only auth-bearing tokens
// have a probe; S3 keys need SigV4 (bucket-scoped) and are reported present-only.
type TokenKind int

const (
	KindNone TokenKind = iota
	kindLinode
	kindGitHub
	kindGHCR
	KindS3
)

func KindFor(name string) TokenKind {
	switch name {
	case "LINODE_API_TOKEN", "LINODE_DNS_TOKEN":
		return kindLinode
	case "OPENBAO_SECRETS_WRITE_TOKEN", "APL_VALUES_REPO_TOKEN", "E2E_DISPATCH_TOKEN":
		return kindGitHub
	case "GHCR_READ_TOKEN":
		return kindGHCR
	case "TF_STATE_ACCESS_KEY", "TF_STATE_SECRET_KEY":
		return KindS3
	default:
		return KindNone
	}
}

// probeToken runs the kind-appropriate validity check for one credential value.
func ProbeToken(name, value, ghcrUser string, now time.Time) TokenValidity {
	switch KindFor(name) {
	case kindLinode:
		code, _ := LinodeProbe(value)
		s, d := classifyAuthStatus(code)
		if s == VValid {
			d = "valid (authenticates to the Linode API)"
		}
		return TokenValidity{name, s, d}
	case kindGitHub:
		code, exp, err := GHPATProbe(envOr("GITHUB_API", "https://api.github.com"), value)
		if err != nil {
			code = 0
		}
		state, days := health.ClassifyPATResponse(true, code, exp, now, 90, 14)
		return githubPATValidity(name, state, days, code)
	case kindGHCR:
		code, _ := GHCRTokenProbe(ghcrUser, value)
		s, d := classifyAuthStatus(code)
		if s == VValid {
			d = "valid (authenticates to the GHCR token endpoint)"
		}
		return TokenValidity{name, s, d}
	case KindS3:
		return TokenValidity{name, VSkipped, "present — not probed (S3 keys need a bucket-scoped SigV4 request)"}
	default:
		return TokenValidity{name, VSkipped, ""}
	}
}

// validityCell renders one verdict as a colored status + detail.
func ValidityCell(tv TokenValidity) string {
	switch tv.Status {
	case VValid:
		return color.Green("✓ " + orDefault(tv.Detail, "valid"))
	case VWarn:
		return color.Yellow("⚠ " + tv.Detail)
	case VInvalid:
		return color.Red("✗ INVALID — " + tv.Detail)
	case VUnreachable:
		return color.Yellow("⚠ " + tv.Detail)
	default: // vSkipped
		return color.Dim("– " + orDefault(tv.Detail, "not probed"))
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
