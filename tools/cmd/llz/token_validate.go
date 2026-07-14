package main

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
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
)

type validityStatus int

const (
	vSkipped     validityStatus = iota // no locally-readable value → not probed
	vValid                             // authenticated cleanly
	vWarn                              // authenticated but flagged (near expiry / policy)
	vInvalid                           // auth rejected (401/403), expired, or revoked
	vUnreachable                       // endpoint unreachable → couldn't verify
)

// tokenValidity is one credential's probe outcome.
type tokenValidity struct {
	name   string
	status validityStatus
	detail string
}

// ── HTTP probe seams (package vars so tests run without network) ──────────────

// linodeProbe GETs the Linode profile with the PAT as a Bearer token; the status
// (0 = unreachable) tells valid (2xx) from rejected (401/403). Any valid Linode
// token can read /v4/profile, so this confirms the token authenticates.
var linodeProbe = func(token string) (int, error) {
	return bearerProbe("https://api.linode.com/v4/profile", token)
}

// ghcrTokenProbe hits the GHCR token endpoint with Basic auth (user:token) for a
// pull scope — the exact request `ghcrChartPublished` makes, so a 403 here is the
// same 403 that fails the chart pre-flight. Status 0 = unreachable.
var ghcrTokenProbe = func(user, token string) (int, error) {
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
func classifyAuthStatus(code int) (validityStatus, string) {
	switch {
	case code == 0:
		return vUnreachable, "endpoint unreachable — could not verify (not failing on connectivity)"
	case code/100 == 2:
		return vValid, ""
	case code == 401 || code == 403:
		return vInvalid, fmt.Sprintf("auth rejected (HTTP %d) — token invalid, revoked, or expired; rotate it", code)
	default:
		return vUnreachable, fmt.Sprintf("unexpected HTTP %d — could not verify", code)
	}
}

// githubPATValidity turns the shared PAT classifier's verdict into a
// tokenValidity. Authenticating-but-flagged states (near expiry, over the 90-day
// policy, never-expiring) are vWarn — the token WORKS, but should be rotated.
func githubPATValidity(name string, state health.PATCheckState, days, code int) tokenValidity {
	switch state {
	case health.PATInvalid:
		return tokenValidity{name, vInvalid, fmt.Sprintf("GitHub returned %d — token invalid, revoked, or expired; rotate it", code)}
	case health.PATExpired:
		return tokenValidity{name, vInvalid, "expired — rotate it now"}
	case health.PATUnreachable:
		return tokenValidity{name, vUnreachable, "api.github.com unreachable — could not verify"}
	case health.PATNoExpiry:
		return tokenValidity{name, vWarn, "valid, but a never-expiring classic PAT (recreate with a ≤90-day expiry)"}
	case health.PATOverPolicy:
		return tokenValidity{name, vWarn, fmt.Sprintf("valid, but expires in %dd (>90-day policy)", days)}
	case health.PATWarn:
		return tokenValidity{name, vWarn, fmt.Sprintf("valid, expires in %dd — rotate soon", days)}
	case health.PATUnparseable:
		return tokenValidity{name, vWarn, "valid, but expiry header unparseable — verify manually"}
	default: // PATOK
		return tokenValidity{name, vValid, fmt.Sprintf("valid, expires in %dd", days)}
	}
}

// tokenKind selects the probe for a credential name. Only auth-bearing tokens
// have a probe; S3 keys need SigV4 (bucket-scoped) and are reported present-only.
type tokenKind int

const (
	kindNone tokenKind = iota
	kindLinode
	kindGitHub
	kindGHCR
	kindS3
)

func kindFor(name string) tokenKind {
	switch name {
	case "LINODE_API_TOKEN", "LINODE_DNS_TOKEN":
		return kindLinode
	case "OPENBAO_SECRETS_WRITE_TOKEN", "APL_VALUES_REPO_TOKEN", "E2E_DISPATCH_TOKEN":
		return kindGitHub
	case "GHCR_READ_TOKEN":
		return kindGHCR
	case "TF_STATE_ACCESS_KEY", "TF_STATE_SECRET_KEY":
		return kindS3
	default:
		return kindNone
	}
}

// probeToken runs the kind-appropriate validity check for one credential value.
func probeToken(name, value, ghcrUser string, now time.Time) tokenValidity {
	switch kindFor(name) {
	case kindLinode:
		code, _ := linodeProbe(value)
		s, d := classifyAuthStatus(code)
		if s == vValid {
			d = "valid (authenticates to the Linode API)"
		}
		return tokenValidity{name, s, d}
	case kindGitHub:
		code, exp, err := ghPATProbe(envOr("GITHUB_API", "https://api.github.com"), value)
		if err != nil {
			code = 0
		}
		state, days := health.ClassifyPATResponse(true, code, exp, now, 90, 14)
		return githubPATValidity(name, state, days, code)
	case kindGHCR:
		code, _ := ghcrTokenProbe(ghcrUser, value)
		s, d := classifyAuthStatus(code)
		if s == vValid {
			d = "valid (authenticates to the GHCR token endpoint)"
		}
		return tokenValidity{name, s, d}
	case kindS3:
		return tokenValidity{name, vSkipped, "present — not probed (S3 keys need a bucket-scoped SigV4 request)"}
	default:
		return tokenValidity{name, vSkipped, ""}
	}
}

// probeTokenValidities probes every probeable requirement and returns a verdict
// keyed by credential NAME, plus the count of INVALID ones. It does NOT print —
// reportReadiness renders the results as the table's VALID column. A probeable
// token with no locally-readable value gets a vSkipped verdict (probe it in CI);
// non-credential requirements (plain vars, image refs) get no entry.
func probeTokenValidities(reqs []requirement, secrets, vars map[string]string, instance liveState, ghcrUser string) (map[string]tokenValidity, int) {
	now := time.Now()
	out := map[string]tokenValidity{}

	// The OBJ state-bucket key PAIR is validated together (both keys + endpoint +
	// bucket, via SigV4); the one verdict is mirrored onto both rows so neither
	// shows a bare N/A. Values come from the local .llz cache.
	endpoint := firstNonEmpty(vars["TF_STATE_ENDPOINT"], instance.value("TF_STATE_ENDPOINT"))
	bucket := firstNonEmpty(vars["TF_STATE_BUCKET"], instance.value("TF_STATE_BUCKET"))
	s3v := probeS3Pair(secrets["TF_STATE_ACCESS_KEY"], secrets["TF_STATE_SECRET_KEY"], endpoint, bucket)

	invalid := 0
	for _, r := range reqs {
		k := kindFor(r.Name)
		if k == kindNone {
			continue // not a probeable credential (plain vars, image refs, …)
		}
		if k == kindS3 {
			tv := s3v
			tv.name = r.Name
			// No local value but set on GitHub → clarify it's a cache miss, not absent.
			if tv.status == vSkipped && strings.HasPrefix(tv.detail, "not cached") && instance.has(r.Name, true) {
				tv.detail = "set on GitHub — not in .llz cache; gather locally or use `llz ci validate-tokens`"
			}
			out[r.Name] = tv
			if r.Name == "TF_STATE_ACCESS_KEY" && tv.status == vInvalid {
				invalid++ // count the pair once
			}
			continue
		}
		val, haveLocal := localValue(r, secrets, vars)
		if !haveLocal {
			// No local value: distinguish "set on GitHub, just not cached" from
			// "not configured anywhere" — neither is a bare N/A.
			if instance.has(r.Name, r.Secret) {
				out[r.Name] = tokenValidity{r.Name, vSkipped, "set on GitHub — not in .llz cache; gather locally or use `llz ci validate-tokens`"}
			} else {
				out[r.Name] = tokenValidity{r.Name, vSkipped, "not set"}
			}
			continue
		}
		tv := probeToken(r.Name, val, ghcrUser, now)
		if tv.status == vInvalid {
			invalid++
		}
		out[r.Name] = tv
	}
	return out, invalid
}

// localValue returns a requirement's value from the local .llz cache (secrets or
// vars, by kind) and whether it was present.
func localValue(r requirement, secrets, vars map[string]string) (string, bool) {
	m := vars
	if r.Secret {
		m = secrets
	}
	v, ok := m[r.Name]
	return v, ok && v != ""
}

// validityCell renders one verdict as a colored status + detail.
func validityCell(tv tokenValidity) string {
	switch tv.status {
	case vValid:
		return green("✓ " + orDefault(tv.detail, "valid"))
	case vWarn:
		return yellow("⚠ " + tv.detail)
	case vInvalid:
		return red("✗ INVALID — " + tv.detail)
	case vUnreachable:
		return yellow("⚠ " + tv.detail)
	default: // vSkipped
		return dim("– " + orDefault(tv.detail, "not probed"))
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
