package forge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// DeleteEnvIdempotent passes on a 404 — and would pass equally if no request were
// ever sent. This pins the request: a DELETE at the variable endpoint, carrying
// the environment_scope filter GitLab keys the variable by (without it GitLab
// would delete the wrong (key, scope) pair, or nothing).
func TestGitLab_DeleteEnvIssuesTheRequest(t *testing.T) {
	var gotMethod, gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotURI = r.Method, r.RequestURI
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := gitlabClient(t, mustGitLab(t), srv.URL).DeleteEnvSecret("prod", "LINODE_API_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE (no request was sent at all if empty)", gotMethod)
	}
	if !strings.Contains(gotURI, "/variables/LINODE_API_TOKEN") || !strings.Contains(gotURI, "environment_scope") {
		t.Errorf("request URI = %q, want the scoped variable endpoint", gotURI)
	}
}

// A failed create must surface. The upsert falls back from PUT-404 to POST, and
// only a 200/201 from that POST means the variable exists; anything else (a 500,
// a 403 from a token without api scope) must be an error, not a silent success
// that leaves CI reading a variable that was never written.
func TestGitLab_UpsertCreateFailureSurfaces(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				w.WriteHeader(http.StatusNotFound) // absent → falls through to create
				return
			}
			w.WriteHeader(code)
		}))
		err := gitlabClient(t, mustGitLab(t), srv.URL).SetRepoSecret("HARBOR_PASSWORD", "s3cr3tvalue")
		srv.Close()
		if err == nil {
			t.Errorf("create returning HTTP %d must be an error, got nil", code)
			continue
		}
		if !strings.Contains(err.Error(), "create variable") {
			t.Errorf("err = %v, want it to name the failed create", err)
		}
	}
	// And a 201 create is still success (the mirror case, so the check above
	// cannot be satisfied by rejecting everything).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	if err := gitlabClient(t, mustGitLab(t), srv.URL).SetRepoSecret("HARBOR_PASSWORD", "s3cr3tvalue"); err != nil {
		t.Errorf("201 create should succeed, got %v", err)
	}
}

// A non-2xx access-token response must never yield a token, even when the body
// parses as one — GitLab error bodies are JSON too, and accepting a token from a
// failed rotate would swap a working credential for a bogus one.
func TestGitLab_TokenOpRejectsNon2xxWithBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "glpat-NOT-REAL"})
	}))
	defer srv.Close()

	tok, err := gitlabClient(t, mustGitLab(t), srv.URL).RotateSelf(3600)
	if err == nil {
		t.Fatalf("HTTP 500 must be an error, got token %q", tok)
	}
	if tok != "" {
		t.Errorf("token = %q, want empty on a failed rotate", tok)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want the status code reported", err)
	}
}

// The GitLab client is used from the same in-cluster CronJobs as the GitHub one;
// a zero timeout turns an unreachable instance into a hung job.
func TestGitLab_ClientTimeoutIsSet(t *testing.T) {
	c, err := NewGitLabClient(mustGitLab(t), "glpat-test", "grp/proj")
	if err != nil {
		t.Fatal(err)
	}
	if to := c.client.Timeout; to <= 0 || to > time.Minute {
		t.Errorf("client timeout = %v, want a bounded non-zero timeout (0 means wait forever)", to)
	}
}
