package forge

// githubFamily implements Forge for all three GitHub kinds. They share every
// convention — sealed-box secrets, x-access-token git auth, a `repository` OIDC
// claim — and differ only in three URLs: the API base, and (for GHES) the OIDC
// issuer/discovery host. Those differences are captured in the struct fields set
// by New, so there is one implementation rather than three near-copies.
type githubFamily struct {
	flavor  Flavor
	host    string // github.com, a ghe.com tenant, or a GHES appliance
	apiBase string
}

func (g githubFamily) Flavor() Flavor  { return g.flavor }
func (g githubFamily) Host() string    { return g.host }
func (g githubFamily) APIBase() string { return g.apiBase }

// GitCredential: GitHub takes any username with the token as the password; the
// conventional sentinel is x-access-token (what an App installation token and a
// PAT both use over HTTPS).
func (g githubFamily) GitCredential(token string) (string, string) {
	return "x-access-token", token
}

func (g githubFamily) RepoURL(slug string) string {
	return "https://" + g.host + "/" + slug + ".git"
}

// OIDC. GHES issues Actions OIDC tokens from https://HOST/_services/token, not
// the github.com issuer — this is the coupling that silently breaks the
// keyless CI→OpenBao path on a GHES instance if the issuer is hardcoded. GHEC
// (even a ghe.com tenant) keeps the github.com issuer; only its API base moves.
func (g githubFamily) OIDC() OIDCConfig {
	issuer := dotComOIDC
	if g.flavor == GHES {
		issuer = "https://" + g.host + "/_services/token"
	}
	return OIDCConfig{
		DiscoveryURL: issuer,
		Issuer:       issuer,
		Audience:     g.audience(),
		// repo identity lives in the `repository` claim across the GitHub family;
		// the value is filled in per-role by the caller (it needs the slug).
		BoundClaims: map[string]string{"repository": ""},
	}
}

// audience mirrors the value `llz ci bao-configure` pins today: the owner's
// GitHub-OIDC default audience. For GHES it is the appliance host.
func (g githubFamily) audience() string {
	if g.flavor == GHES {
		return "https://" + g.host
	}
	return "https://github.com" // owner is appended by BoundClaimsFor
}

// gitlab implements Forge for a GitLab instance (self-managed or gitlab.com,
// distinguished only by host). It differs from the GitHub family in every
// pure dimension: /api/v4, oauth2 git auth, an instance-domain OIDC issuer, and
// a `project_path` identity claim instead of `repository`.
type gitlab struct {
	host string
}

func (l gitlab) Flavor() Flavor  { return GitLab }
func (l gitlab) Host() string    { return l.host }
func (l gitlab) APIBase() string { return "https://" + l.host + "/api/v4" }

// GitCredential: GitLab HTTPS git auth uses the literal username "oauth2" with
// the token as the password (project/personal access tokens both work this way).
func (l gitlab) GitCredential(token string) (string, string) {
	return "oauth2", token
}

func (l gitlab) RepoURL(slug string) string {
	return "https://" + l.host + "/" + slug + ".git"
}

// OIDC. A GitLab ID token's issuer and audience default to the instance domain,
// and it carries no `repository` claim — repo identity is `project_path`
// (group/project). An OpenBao jwt role bound to a GitHub-shaped claim map would
// never match a GitLab token; this is why OIDCConfig yields the claim map.
func (l gitlab) OIDC() OIDCConfig {
	base := "https://" + l.host
	return OIDCConfig{
		DiscoveryURL: base,
		Issuer:       base,
		Audience:     base,
		BoundClaims:  map[string]string{"project_path": ""},
	}
}

// BoundClaimsFor returns f's OIDC bound-claims map with the repo-identity claim
// filled in for slug ("<owner>/<name>" on GitHub, "<group>/<project>" on
// GitLab). It also finalizes the audience where the owner is part of it (the
// GitHub family appends the owner to https://github.com/). This is the value an
// OpenBao jwt role's bound_claims should be set to.
func BoundClaimsFor(f Forge, slug string) map[string]string {
	out := map[string]string{}
	for k := range f.OIDC().BoundClaims {
		out[k] = slug
	}
	return out
}

// AudienceFor returns f's OIDC audience finalized for a repo owner. On the
// GitHub family (non-GHES) the audience is https://github.com/<owner>; elsewhere
// the base audience already stands alone.
func AudienceFor(f Forge, owner string) string {
	if gf, ok := f.(githubFamily); ok && gf.flavor != GHES {
		return "https://github.com/" + owner
	}
	return f.OIDC().Audience
}
