package forge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func gitlabClient(t *testing.T, apiBaseHost Forge, srvURL string) *GitLabClient {
	t.Helper()
	c, err := NewGitLabClient(apiBaseHost, "glpat-test", "grp/proj")
	if err != nil {
		t.Fatal(err)
	}
	c.apiBase = srvURL // point at the test server
	c.now = func() time.Time { return time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC) }
	return c
}

func mustGitLab(t *testing.T) Forge {
	f, err := New(GitLab, "gitlab.corp")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestNewGitLabClient_RejectsNonGitLab(t *testing.T) {
	gh, _ := New(GitHub, "")
	if _, err := NewGitLabClient(gh, "t", "g/p"); err == nil {
		t.Error("NewGitLabClient must reject a GitHub forge")
	}
}

// A masked, project-wide variable: PUT (update) then POST (create) on 404, with
// the URL-encoded project path and environment_scope carried through.
func TestGitLab_SetRepoSecretUpsert(t *testing.T) {
	var methods []string
	var gotProjectInPath bool
	var createForm map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if strings.Contains(r.RequestURI, "grp%2Fproj") {
			gotProjectInPath = true
		}
		if r.Header.Get("PRIVATE-TOKEN") != "glpat-test" {
			t.Errorf("PRIVATE-TOKEN = %q", r.Header.Get("PRIVATE-TOKEN"))
		}
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusNotFound) // not there yet
		case http.MethodPost:
			_ = r.ParseForm()
			createForm = map[string]string{
				"key": r.PostFormValue("key"), "value": r.PostFormValue("value"),
				"masked": r.PostFormValue("masked"), "environment_scope": r.PostFormValue("environment_scope"),
			}
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	if err := gitlabClient(t, mustGitLab(t), srv.URL).SetRepoSecret("HARBOR_PASSWORD", "s3cr3tvalue"); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 || methods[0] != http.MethodPut || methods[1] != http.MethodPost {
		t.Errorf("methods = %v, want [PUT POST]", methods)
	}
	if !gotProjectInPath {
		t.Error("project path was not URL-encoded (grp%2Fproj) in the request")
	}
	if createForm["key"] != "HARBOR_PASSWORD" || createForm["masked"] != "true" || createForm["environment_scope"] != "*" {
		t.Errorf("create form = %v", createForm)
	}
}

func TestGitLab_SetVariableUnmasked(t *testing.T) {
	var masked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK) // exists → update succeeds
			_ = r.ParseForm()
			masked = r.PostFormValue("masked")
		}
	}))
	defer srv.Close()
	if err := gitlabClient(t, mustGitLab(t), srv.URL).SetVariable("APPS_REVISION", "abc"); err != nil {
		t.Fatal(err)
	}
	if masked != "false" {
		t.Errorf("masked = %q, want false for a plain variable", masked)
	}
}

func TestGitLab_DeleteEnvIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if err := gitlabClient(t, mustGitLab(t), srv.URL).DeleteEnvSecret("prod", "GONE"); err != nil {
		t.Errorf("delete of absent variable should succeed, got %v", err)
	}
}

// RotateSelf hits the self/rotate endpoint with a computed expires_at and returns
// the new token — the self-renewing path that needs no permanent root secret.
func TestGitLab_RotateSelf(t *testing.T) {
	var gotPath, gotExpires string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = r.ParseForm()
		gotExpires = r.PostFormValue("expires_at")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "glpat-NEW"})
	}))
	defer srv.Close()

	tok, err := gitlabClient(t, mustGitLab(t), srv.URL).RotateSelf(90 * 24 * 3600)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "glpat-NEW" {
		t.Errorf("rotated token = %q", tok)
	}
	if !strings.HasSuffix(gotPath, "/access_tokens/self/rotate") {
		t.Errorf("path = %q, want .../access_tokens/self/rotate", gotPath)
	}
	if gotExpires != "2026-10-15" { // 2026-07-17 + 90d
		t.Errorf("expires_at = %q, want 2026-10-15 (now+90d)", gotExpires)
	}
}

func TestGitLab_MintEphemeralSendsScopes(t *testing.T) {
	var scopes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		scopes = r.PostForm["scopes[]"]
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "glpat-EPH"})
	}))
	defer srv.Close()
	tok, err := gitlabClient(t, mustGitLab(t), srv.URL).MintEphemeral([]string{"api", "self_rotate"}, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "glpat-EPH" {
		t.Errorf("minted token = %q", tok)
	}
	if strings.Join(scopes, ",") != "api,self_rotate" {
		t.Errorf("scopes = %v", scopes)
	}
}

func TestGitLab_TokenExpiryParsesDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "glpat-probed" {
			t.Errorf("introspection must auth as the probed token, got %q", r.Header.Get("PRIVATE-TOKEN"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"expires_at": "2026-09-01"})
	}))
	defer srv.Close()
	got, err := gitlabClient(t, mustGitLab(t), srv.URL).TokenExpiry("glpat-probed")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix()
	if got != want {
		t.Errorf("expiry = %d, want %d", got, want)
	}
}

// A GitHub forge must NOT satisfy TokenRotator; a GitLab client must — the
// asymmetry the interface split encodes.
func TestRotator_InterfaceAsymmetry(t *testing.T) {
	gh, _ := New(GitHub, "")
	if _, ok := interface{}(gh).(TokenRotator); ok {
		t.Error("GitHub forge must not implement TokenRotator")
	}
	var c interface{} = &GitLabClient{}
	if _, ok := c.(TokenRotator); !ok {
		t.Error("GitLabClient must implement TokenRotator")
	}
}
