package forge

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// The GitHub role body must stay byte-identical to the string
// ci_openbao_configure.go hardcoded before Phase 3, so wiring the bootstrap to
// this helper is a no-op on the only forge wired end-to-end today.
func TestOpenBaoJWTRoleBody_GitHubUnchanged(t *testing.T) {
	f, _ := New(GitHub, "")
	got, err := OpenBaoJWTRoleBody(f, "acme/platform", "platform-ci")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(
		`{"role_type":"jwt","user_claim":"sub","bound_audiences":["https://github.com/%s"],"bound_claims":{"repository":"%s"},"token_policies":["%s"],"token_ttl":"15m","token_max_ttl":"30m"}`,
		"acme", "acme/platform", "platform-ci")
	if got != want {
		t.Errorf("GitHub role body drifted from the pre-Phase-3 hardcoded form:\n got: %s\nwant: %s", got, want)
	}
}

func TestOpenBaoJWTAuthConfig(t *testing.T) {
	gh, _ := New(GitHub, "")
	d, iss := OpenBaoJWTAuthConfig(gh)
	if d != "https://token.actions.githubusercontent.com" || iss != d {
		t.Errorf("github jwt config = (%q,%q)", d, iss)
	}
	ghes, _ := New(GHES, "ghes.corp")
	d, iss = OpenBaoJWTAuthConfig(ghes)
	if d != "https://ghes.corp/_services/token" || iss != d {
		t.Errorf("GHES jwt config = (%q,%q), want the appliance issuer", d, iss)
	}
}

// GHES and GitLab role bodies must carry the right issuer-shaped audience and
// the right identity claim — the whole point of making the role forge-shaped.
func TestOpenBaoJWTRoleBody_NonGitHub(t *testing.T) {
	type claims struct {
		BoundAudiences []string          `json:"bound_audiences"`
		BoundClaims    map[string]string `json:"bound_claims"`
	}
	ghes, _ := New(GHES, "ghes.corp")
	body, _ := OpenBaoJWTRoleBody(ghes, "acme/platform", "platform-ci")
	var c claims
	_ = json.Unmarshal([]byte(body), &c)
	if !reflect.DeepEqual(c.BoundAudiences, []string{"https://ghes.corp"}) {
		t.Errorf("GHES bound_audiences = %v", c.BoundAudiences)
	}
	if c.BoundClaims["repository"] != "acme/platform" {
		t.Errorf("GHES bound_claims = %v", c.BoundClaims)
	}

	gl, _ := New(GitLab, "gitlab.corp")
	body, _ = OpenBaoJWTRoleBody(gl, "grp/proj", "platform-ci")
	c = claims{}
	_ = json.Unmarshal([]byte(body), &c)
	if !reflect.DeepEqual(c.BoundAudiences, []string{"https://gitlab.corp"}) {
		t.Errorf("GitLab bound_audiences = %v", c.BoundAudiences)
	}
	if _, ok := c.BoundClaims["repository"]; ok {
		t.Errorf("GitLab must bind project_path, not repository: %v", c.BoundClaims)
	}
	if c.BoundClaims["project_path"] != "grp/proj" {
		t.Errorf("GitLab bound_claims = %v", c.BoundClaims)
	}
}
