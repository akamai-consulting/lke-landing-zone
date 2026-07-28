package linode

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The reset endpoint is the one irreversible call in this package, so pin the
// exact method and path: a POST to the wrong URL that happens to 200 would be
// reported as a successful rotation that never happened.
func TestResetPostgresCredentials_PathAndMethod(t *testing.T) {
	var gotMethod, gotPath string
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		writeJSON(w, 200, map[string]any{})
	})
	if err := c.ResetPostgresCredentials(context.Background(), 12345); err != nil {
		t.Fatalf("ResetPostgresCredentials: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if want := "/v4/databases/postgresql/instances/12345/credentials/reset"; gotPath != want {
		t.Errorf("path = %s, want %s", gotPath, want)
	}
}

// A non-2xx must be an error, and must carry the body — a silent failure here
// would let the rotator stamp rotated_at on a credential it never changed.
func TestResetPostgresCredentials_ErrorCarriesBody(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 400, map[string]any{"errors": []map[string]string{{"reason": "database busy"}}})
	})
	err := c.ResetPostgresCredentials(context.Background(), 7)
	if err == nil {
		t.Fatal("a 400 must be an error")
	}
	if !strings.Contains(err.Error(), "database busy") {
		t.Errorf("error should carry the API body, got: %v", err)
	}
}

func TestPostgresCredentials(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if want := "/v4/databases/postgresql/instances/99/credentials"; r.URL.Path != want {
			t.Errorf("path = %s, want %s", r.URL.Path, want)
		}
		writeJSON(w, 200, map[string]any{"username": "akmadmin", "password": "s3cr3t"})
	})
	creds, err := c.PostgresCredentials(context.Background(), 99)
	if err != nil {
		t.Fatalf("PostgresCredentials: %v", err)
	}
	if creds.Username != "akmadmin" || creds.Password != "s3cr3t" {
		t.Errorf("creds = %+v", creds)
	}
}

func TestPostgresCredentials_Non2xxIsError(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) })
	if _, err := c.PostgresCredentials(context.Background(), 1); err == nil {
		t.Error("a 403 must be an error, not empty credentials")
	}
}

// The rotator branches on `status` to decide the reset has landed, so the field
// has to survive decoding — including the numeric members json.Number preserves.
func TestPostgresInstance_Status(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if want := "/v4/databases/postgresql/instances/42"; r.URL.Path != want {
			t.Errorf("path = %s, want %s", r.URL.Path, want)
		}
		writeJSON(w, 200, map[string]any{"id": 42, "status": "updating", "label": "platform-shared-prod"})
	})
	inst, err := c.PostgresInstance(context.Background(), 42)
	if err != nil {
		t.Fatalf("PostgresInstance: %v", err)
	}
	if inst["status"] != "updating" {
		t.Errorf("status = %v, want updating", inst["status"])
	}
}

func TestPostgresInstance_Non2xxIsError(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	if _, err := c.PostgresInstance(context.Background(), 1); err == nil {
		t.Error("a 404 must be an error")
	}
}

func TestListPostgresInstances(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v4/databases/postgresql/instances") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{
			"data":  []map[string]any{{"id": 1, "label": "platform-shared-prod"}},
			"page":  1,
			"pages": 1,
		})
	})
	got, err := c.ListPostgresInstances(context.Background())
	if err != nil {
		t.Fatalf("ListPostgresInstances: %v", err)
	}
	if len(got) != 1 || got[0]["label"] != "platform-shared-prod" {
		t.Errorf("got %v", got)
	}
}
