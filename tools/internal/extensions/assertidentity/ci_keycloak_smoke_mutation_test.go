package assertidentity

// ci_keycloak_smoke_mutation_test.go — status handling in the smoke's realm
// writes and teardown. Mutation testing showed these checks could be made inert
// without a single test noticing: a failed teardown would then read as clean
// while a PUBLIC, ROPC-enabled, aud:llz-stamped client (or a live team-member
// user) stays standing in the realm. Each helper gets a table over its EXACT
// accepted status set plus a rejected one.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// kcStatusServer answers exactly one method+path with `status` (plus a short
// error body), counting hits; every other request fails the test. It lets a
// status table drive one helper's accept/reject decision with no network.
func kcStatusServer(t *testing.T, method, path string, status int) (*httptest.Server, *int) {
	t.Helper()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method || r.URL.Path != path {
			t.Errorf("unexpected %s %s (want %s %s)", r.Method, r.URL.Path, method, path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		hits++
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"errorMessage":"boom"}`))
	}))
	return srv, &hits
}

// TestEnsureDirectGrantClient_PropagatesAudienceMapperFailure: the smoke client's
// tokens only satisfy OpenBao's bound_audiences because of the aud:llz mapper. If
// stamping it fails and the error is swallowed, the smoke goes on to blame the
// OpenBao role for the login failure the mapper caused.
