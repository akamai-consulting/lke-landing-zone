package forge

import "testing"

func TestNew_CoreURLs(t *testing.T) {
	cases := []struct {
		name     string
		flavor   Flavor
		host     string
		wantErr  bool
		apiBase  string
		repoURL  string
		gitUser  string
		wantHost string
	}{
		{name: "github.com", flavor: GitHub, apiBase: "https://api.github.com",
			repoURL: "https://github.com/o/i.git", gitUser: "x-access-token", wantHost: "github.com"},
		{name: "ghec default", flavor: GHEC, apiBase: "https://api.github.com",
			repoURL: "https://github.com/o/i.git", gitUser: "x-access-token", wantHost: "github.com"},
		{name: "ghec tenant", flavor: GHEC, host: "octo.ghe.com", apiBase: "https://api.octo.ghe.com",
			repoURL: "https://octo.ghe.com/o/i.git", gitUser: "x-access-token", wantHost: "octo.ghe.com"},
		{name: "ghes", flavor: GHES, host: "ghes.corp", apiBase: "https://ghes.corp/api/v3",
			repoURL: "https://ghes.corp/o/i.git", gitUser: "x-access-token", wantHost: "ghes.corp"},
		{name: "gitlab", flavor: GitLab, host: "gitlab.corp", apiBase: "https://gitlab.corp/api/v4",
			repoURL: "https://gitlab.corp/o/i.git", gitUser: "oauth2", wantHost: "gitlab.corp"},
		{name: "ghes needs host", flavor: GHES, wantErr: true},
		{name: "gitlab needs host", flavor: GitLab, wantErr: true},
		{name: "unknown", flavor: Flavor("bitbucket"), wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := New(c.flavor, c.host)
			if c.wantErr {
				if err == nil {
					t.Fatalf("New(%q,%q) = nil err, want error", c.flavor, c.host)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%q,%q) unexpected error: %v", c.flavor, c.host, err)
			}
			if got := f.APIBase(); got != c.apiBase {
				t.Errorf("APIBase = %q, want %q", got, c.apiBase)
			}
			if got := f.RepoURL("o/i"); got != c.repoURL {
				t.Errorf("RepoURL = %q, want %q", got, c.repoURL)
			}
			if got := f.Host(); got != c.wantHost {
				t.Errorf("Host = %q, want %q", got, c.wantHost)
			}
			u, p := f.GitCredential("TOK")
			if u != c.gitUser || p != "TOK" {
				t.Errorf("GitCredential = (%q,%q), want (%q,TOK)", u, p, c.gitUser)
			}
		})
	}
}

func TestOIDC(t *testing.T) {
	// GitHub.com and GHEC share the github.com issuer and the `repository` claim;
	// audience is owner-qualified.
	for _, fl := range []Flavor{GitHub, GHEC} {
		f, _ := New(fl, "")
		o := f.OIDC()
		if o.Issuer != "https://token.actions.githubusercontent.com" {
			t.Errorf("%s issuer = %q", fl, o.Issuer)
		}
		if _, ok := o.BoundClaims["repository"]; !ok {
			t.Errorf("%s missing repository claim: %v", fl, o.BoundClaims)
		}
		if got := AudienceFor(f, "acme"); got != "https://github.com/acme" {
			t.Errorf("%s AudienceFor = %q", fl, got)
		}
		if bc := BoundClaimsFor(f, "acme/inst"); bc["repository"] != "acme/inst" {
			t.Errorf("%s BoundClaimsFor = %v", fl, bc)
		}
	}

	// GHES issues from its own host and audiences to itself.
	g, _ := New(GHES, "ghes.corp")
	o := g.OIDC()
	if o.Issuer != "https://ghes.corp/_services/token" {
		t.Errorf("GHES issuer = %q", o.Issuer)
	}
	if got := AudienceFor(g, "acme"); got != "https://ghes.corp" {
		t.Errorf("GHES AudienceFor = %q, want the appliance host (owner not appended)", got)
	}

	// GitLab: instance-domain issuer and a project_path claim, NOT repository.
	l, _ := New(GitLab, "gitlab.corp")
	lo := l.OIDC()
	if lo.Issuer != "https://gitlab.corp" {
		t.Errorf("GitLab issuer = %q", lo.Issuer)
	}
	if _, ok := lo.BoundClaims["repository"]; ok {
		t.Errorf("GitLab must not carry a repository claim: %v", lo.BoundClaims)
	}
	if bc := BoundClaimsFor(l, "grp/proj"); bc["project_path"] != "grp/proj" {
		t.Errorf("GitLab BoundClaimsFor = %v, want project_path=grp/proj", bc)
	}
}

func TestSupported(t *testing.T) {
	if err := Supported(GitHub); err != nil {
		t.Errorf("github should be supported: %v", err)
	}
	for _, fl := range []Flavor{GHEC, GHES, GitLab} {
		if err := Supported(fl); err == nil {
			t.Errorf("%s should report not-yet-supported, got nil", fl)
		}
	}
	if err := Supported(Flavor("bitbucket")); err == nil {
		t.Error("unknown flavor should error")
	}
}

func TestRotationSupported(t *testing.T) {
	// The asymmetry that justifies TokenRotator being a separate interface:
	// only GitLab can rotate its root credential via API.
	if !RotationSupported(GitLab) {
		t.Error("GitLab should support rotation")
	}
	for _, fl := range []Flavor{GitHub, GHEC, GHES} {
		if RotationSupported(fl) {
			t.Errorf("%s (GitHub family) must not claim API rotation", fl)
		}
	}
}
