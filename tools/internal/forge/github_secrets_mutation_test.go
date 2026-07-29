package forge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// DeleteEnvSecret's idempotence test passes on a 404 — and would pass just as
// happily if the request were never sent at all. This pins the request itself:
// a DELETE, at the numeric-id environment endpoint, for the named secret. A
// silent no-op here leaves a stale env secret behind after teardown.
func TestGitHubSecretWriter_DeleteEnvIssuesTheRequest(t *testing.T) {
	type call struct{ method, path string }
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, call{r.Method, r.URL.Path})
		if r.URL.Path == "/repos/acme/platform" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := mustWriter(t, srv.URL).DeleteEnvSecret("infra-primary", "LINODE_API_TOKEN"); err != nil {
		t.Fatal(err)
	}
	want := call{http.MethodDelete, "/repositories/7/environments/infra-primary/secrets/LINODE_API_TOKEN"}
	var saw bool
	for _, c := range calls {
		if c == want {
			saw = true
		}
	}
	if !saw {
		t.Errorf("calls = %v, want a %s %s among them", calls, want.method, want.path)
	}
}

// The writer runs in-cluster (Harbor robot provisioner, broad-PAT rotator) on the
// distroless image; a zero client timeout means a hung GitHub connection blocks
// the CronJob indefinitely instead of failing and retrying.
func TestGitHubSecretWriter_ClientTimeoutIsSet(t *testing.T) {
	w := mustWriter(t, "https://api.github.com")
	if to := w.client.Timeout; to <= 0 || to > time.Minute {
		t.Errorf("client timeout = %v, want a bounded non-zero timeout (0 means wait forever)", to)
	}
}
