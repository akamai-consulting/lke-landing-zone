package forge

// openbao_oidc.go — emit the OpenBao jwt auth-method config and role bodies from
// a Forge. This is the Phase 3 seam: ci_openbao_configure.go hardcoded the
// github.com issuer, the https://github.com/<owner> audience, and a
// {"repository": …} bound-claim; those are now forge-derived, so a GHES instance
// gets its /_services/token issuer and a GitLab instance gets a project_path
// claim without the bootstrap code knowing which forge it is talking to.
//
// The GitHub output is byte-equivalent to the previous hardcoded config (locked
// by openbao_oidc_test.go), so this is a behavior-preserving change on the only
// forge wired end-to-end today.

import (
	"encoding/json"
	"strings"
)

// OpenBaoJWTAuthConfig returns the (oidc_discovery_url, bound_issuer) pair for
// `bao write auth/jwt/config`.
func OpenBaoJWTAuthConfig(f Forge) (discoveryURL, boundIssuer string) {
	o := f.OIDC()
	return o.DiscoveryURL, o.Issuer
}

// OpenBaoJWTRoleBody returns the JSON body for `bao write auth/jwt/role/<name> -`,
// binding a role to slug ("<owner>/<name>" or "<group>/<project>") under the
// given OpenBao policy. bound_audiences is owner-derived; bound_claims carries
// the forge's repo-identity claim (repository on GitHub, project_path on GitLab),
// so a token from any other repo/project cannot mint against this role.
func OpenBaoJWTRoleBody(f Forge, slug, policy string) (string, error) {
	owner := slug
	if i := strings.IndexByte(slug, '/'); i > 0 {
		owner = slug[:i]
	}
	body := struct {
		RoleType       string            `json:"role_type"`
		UserClaim      string            `json:"user_claim"`
		BoundAudiences []string          `json:"bound_audiences"`
		BoundClaims    map[string]string `json:"bound_claims"`
		TokenPolicies  []string          `json:"token_policies"`
		TokenTTL       string            `json:"token_ttl"`
		TokenMaxTTL    string            `json:"token_max_ttl"`
	}{
		RoleType:       "jwt",
		UserClaim:      "sub",
		BoundAudiences: []string{AudienceFor(f, owner)},
		BoundClaims:    BoundClaimsFor(f, slug),
		TokenPolicies:  []string{policy},
		TokenTTL:       "15m",
		TokenMaxTTL:    "30m",
	}
	b, err := json.Marshal(body)
	return string(b), err
}
