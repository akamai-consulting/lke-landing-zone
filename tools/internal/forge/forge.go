// Package forge abstracts the git-forge an instance is hosted on — GitHub.com,
// GitHub Enterprise Cloud (GHEC), GitHub Enterprise Server (GHES), or GitLab.
// It is the package the (previously aspirational) spec.instance.forge field and
// its docstrings referred to; see docs/designs/forge-abstraction.md.
//
// Scope note. This package owns the *pure* forge differences — API base URLs,
// git-credential conventions, and the OIDC issuer/audience/claim shapes an
// OpenBao jwt-role needs. Those are computable from (flavor, host) alone, with
// no network access, and are fully covered here. The capability interfaces
// (SecretWriter, TokenMinter, TokenRotator, ExpiryProber) describe the network
// operations that differ per forge; their implementations land per-phase as the
// rollout in the design doc reaches each forge. A forge that cannot perform a
// capability simply does not implement its interface, so callers probe with a
// type assertion rather than handling an ErrUnsupported at runtime — see
// RotationSupported for the one asymmetry that matters today.
//
// Import direction: this package depends only on the standard library and on
// internal/validate (for the canonical flavor strings). It must never import
// internal/clusterspec — the constructors take primitives (flavor, host), not a
// spec type, precisely so clusterspec can depend on forge without a cycle.
package forge

import (
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/validate"
)

// Flavor is the forge kind. The string values are the wire values accepted in
// spec.instance.forge and validated by validate.Forge; they are re-exported
// from internal/validate so there is exactly one source of truth for the set.
type Flavor string

const (
	GitHub     Flavor = Flavor(validate.ForgeGitHub)                 // github.com
	GHEC       Flavor = Flavor(validate.ForgeGitHubEnterprise)       // Enterprise Cloud
	GHES       Flavor = Flavor(validate.ForgeGitHubEnterpriseServer) // Enterprise Server
	GitLab     Flavor = Flavor(validate.ForgeGitLab)
	githubCom         = "github.com"
	dotComAPI         = "https://api.github.com"
	dotComOIDC        = "https://token.actions.githubusercontent.com"
)

// Forge is the always-present core every flavor implements. Everything beyond
// this is a probed capability (see package doc).
type Forge interface {
	// Flavor identifies the forge kind.
	Flavor() Flavor
	// Host is the forge's web/git host (github.com, a GHES appliance, a GitLab
	// host). It is the authority in RepoURL and, for the non-dotcom flavors,
	// the base for APIBase and the OIDC endpoints.
	Host() string
	// APIBase is the REST API root, e.g. https://api.github.com or
	// https://ghes.corp/api/v3 or https://gitlab.corp/api/v4.
	APIBase() string
	// GitCredential returns the (username, password) pair for HTTPS git auth
	// given a token: ("x-access-token", tok) on the GitHub family, ("oauth2",
	// tok) on GitLab. This is the single choke point that ci_bootstrap_cluster's
	// authedGitURL should consult.
	GitCredential(token string) (user, pass string)
	// RepoURL builds the HTTPS clone URL for an "<owner>/<name>" slug.
	RepoURL(slug string) string
	// OIDC returns the CI→OpenBao OIDC configuration for this forge.
	OIDC() OIDCConfig
}

// OIDCConfig is the forge-shaped input to an OpenBao jwt auth role. The GitHub
// family and GitLab differ not just in issuer host but in the *claim* that
// carries repo identity (`repository` vs `project_path`) — so this yields the
// whole bound-claims map, not a hostname. See ci_openbao_configure.go, which is
// the intended consumer.
type OIDCConfig struct {
	// DiscoveryURL is oidc_discovery_url on the jwt role.
	DiscoveryURL string
	// Issuer is bound_issuer on the jwt role.
	Issuer string
	// Audience is the bound_audiences entry, derived from the repo owner.
	Audience string
	// BoundClaims maps claim name → expected value, e.g.
	// {"repository": "owner/name"} on GitHub or
	// {"project_path": "group/project"} on GitLab.
	BoundClaims map[string]string
}

// SecretWriter is the CI secret plane. Implemented by all four forges, by very
// different means: the GitHub family seals values with a libsodium box and PUTs
// them (see cmd/llz/gh_secrets_native.go); GitLab POSTs a plaintext masked
// CI/CD variable. Wired per-phase.
type SecretWriter interface {
	SetRepoSecret(name, value string) error
	SetEnvSecret(env, name, value string) error
	DeleteEnvSecret(env, name string) error
	SetVariable(name, value string) error
}

// TokenMinter mints a short-lived service credential — a GitHub App installation
// token (≤1h) or a GitLab project/group access token. Wired per-phase.
type TokenMinter interface {
	MintEphemeral(scopes []string, ttlSeconds int) (string, error)
}

// TokenRotator is implemented by GitLab ONLY. The GitHub family cannot rotate
// its root credential (a GitHub App private key) via API — that is a web-UI-only
// flow on github.com and GHES alike — so it must not pretend to. Callers probe:
//
//	if r, ok := f.(forge.TokenRotator); ok { r.RotateSelf(...) } else { /* escrow + alert */ }
//
// See RotationSupported for the pure predicate and the design doc's
// "rotation asymmetry" section for why this is a separate interface.
type TokenRotator interface {
	RotateSelf(ttlSeconds int) (string, error)
	Rotate(tokenID string, ttlSeconds int) (string, error)
}

// ExpiryProber reports when a token expires. GitHub reads the
// GitHub-Authentication-Token-Expiration response header; GitLab reads its
// token-info API. Wired per-phase.
type ExpiryProber interface {
	TokenExpiry(token string) (unixSeconds int64, err error)
}

// New constructs the Forge for a (flavor, host) pair. host is required for GHES
// and GitLab (there is no default appliance) and optional for GHEC (empty means
// a github.com-hosted enterprise; a value means a ghe.com data-residency
// tenant). host is ignored for github.com. It returns an error for an unknown
// flavor or a missing-but-required host, so a bad spec fails at construction
// rather than at first API call.
func New(flavor Flavor, host string) (Forge, error) {
	switch flavor {
	case GitHub:
		return githubFamily{flavor: GitHub, host: githubCom, apiBase: dotComAPI}, nil
	case GHEC:
		if host == "" || host == githubCom {
			return githubFamily{flavor: GHEC, host: githubCom, apiBase: dotComAPI}, nil
		}
		// ghe.com data-residency tenant: API base moves, OIDC issuer does not.
		return githubFamily{flavor: GHEC, host: host, apiBase: "https://api." + host}, nil
	case GHES:
		if host == "" {
			return nil, fmt.Errorf("forge %q requires a host (the GHES appliance, e.g. ghes.corp.example)", flavor)
		}
		return githubFamily{flavor: GHES, host: host, apiBase: "https://" + host + "/api/v3"}, nil
	case GitLab:
		if host == "" {
			return nil, fmt.Errorf("forge %q requires a host (the GitLab instance, e.g. gitlab.corp.example)", flavor)
		}
		return gitlab{host: host}, nil
	default:
		return nil, fmt.Errorf("unknown forge flavor %q (want %s|%s|%s|%s)", flavor, GitHub, GHEC, GHES, GitLab)
	}
}

// Supported reports whether a forge flavor is wired end-to-end today. This is
// the honesty gate the spec validator applies: the pure logic in this package
// covers all four flavors, but instance provisioning (the workflow layer, the
// in-cluster secret writers, the e2e harness) is only validated for GitHub.com.
// The other flavors are recognized and reserved — not silently ignored, and not
// falsely accepted. Each rollout phase in docs/designs/forge-abstraction.md
// flips one of these on as its harness proves out.
func Supported(flavor Flavor) error {
	switch flavor {
	case GitHub:
		return nil
	case GHEC, GHES, GitLab:
		return fmt.Errorf("forge %q is recognized but not yet supported end-to-end — "+
			"only %q is wired today (see docs/designs/forge-abstraction.md)", flavor, GitHub)
	default:
		return fmt.Errorf("unknown forge flavor %q", flavor)
	}
}

// RotationSupported reports whether the forge can rotate its own root service
// credential via API. False for the GitHub family (App private keys are
// UI-only), true for GitLab (project access tokens have a rotate endpoint and a
// self_rotate scope). Observability uses this to tell "manual rotation pending"
// apart on a mixed fleet: on a GitLab instance that state is a bug; on a GitHub
// instance it is expected.
func RotationSupported(flavor Flavor) bool { return flavor == GitLab }
