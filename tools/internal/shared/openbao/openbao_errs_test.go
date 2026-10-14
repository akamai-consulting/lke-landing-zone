package openbao

// Error-path coverage for the KV v2 client: non-2xx/unparseable reads, the
// read-back-absent and rollback-to-a-prior-version branches, the namespace
// header, and the DualWrite read-back failure path. The happy paths live in
// openbao_test.go; these inject precise failures via per-test handlers.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// handlerClient wires a Client to a one-off httptest server with the given handler.
func handlerClient(t *testing.T, ns string, h http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return New(s.URL, "t", ns, 5*time.Second)
}

func TestReadKVNon2xxAndUnparseable(t *testing.T) {
	ctx := context.Background()

	// 500 on the read → readKV surfaces the HTTP error (not a 404 miss).
	c500 := handlerClient(t, "", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream boom", http.StatusInternalServerError)
	})
	if _, _, err := c500.Get(ctx, "secret/app/x", "k"); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("want HTTP 500 read error, got %v", err)
	}

	// 200 with a non-JSON body → decode error.
	cBad := handlerClient(t, "", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	})
	if _, _, err := cBad.Get(ctx, "secret/app/x", "k"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("want parse error, got %v", err)
	}
}

func TestDataHashAbsentAndReadError(t *testing.T) {
	ctx := context.Background()

	// Absent secret (404) → explicit read-back-absent error.
	c404 := handlerClient(t, "", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	if _, err := c404.DataHash(ctx, "secret/app/x"); err == nil || !strings.Contains(err.Error(), "secret absent") {
		t.Errorf("want secret-absent error, got %v", err)
	}

	// Read failure (500) propagates.
	c500 := handlerClient(t, "", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := c500.DataHash(ctx, "secret/app/x"); err == nil {
		t.Error("want read error from DataHash, got nil")
	}
}

func TestWriteNon2xxAndNamespaceHeader(t *testing.T) {
	ctx := context.Background()

	// Write surfaces a non-2xx as an error.
	c500 := handlerClient(t, "", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	})
	if err := c500.Write(ctx, "secret/app/x", map[string]string{"k": "v"}); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("want HTTP 403 write error, got %v", err)
	}

	// A namespaced client sets X-Vault-Namespace on the request.
	var seenNS string
	cNS := handlerClient(t, "team-a", func(w http.ResponseWriter, r *http.Request) {
		seenNS = r.Header.Get("X-Vault-Namespace")
		http.Error(w, "nf", http.StatusNotFound)
	})
	_, _, _ = cNS.Get(ctx, "secret/app/x", "k")
	if seenNS != "team-a" {
		t.Errorf("X-Vault-Namespace = %q, want team-a", seenNS)
	}
}

// TestDualWriteRollbackToPriorVersion exercises Rollback's priorVersion>0 branch:
// a pre-existing secret is restored (re-posted) when the secondary write fails.
func TestDualWriteRollbackToPriorVersion(t *testing.T) {
	p, s := newFakeBao(t), newFakeBao(t)
	ctx := context.Background()
	const path = "secret/app/keys"

	if err := p.client().Write(ctx, path, map[string]string{"k": "v1"}); err != nil {
		t.Fatal(err)
	}
	s.failWrite = true // secondary 500s on the next write

	err := DualWrite(ctx, p.client(), s.client(), path, map[string]string{"k": "v2"})
	if err == nil || !strings.Contains(err.Error(), "rolled back to v1") {
		t.Fatalf("want rollback-to-v1 error, got %v", err)
	}
	if got := p.latest(path)["k"]; got != "v1" {
		t.Errorf("primary should be restored to v1, got %v", got)
	}
}

// TestDualWritePrimaryReadBackFails covers the primary read-back failure branch:
// both writes succeed, but the primary 500s on the post-write hash read.
func TestDualWritePrimaryReadBackFails(t *testing.T) {
	ctx := context.Background()
	const path = "secret/app/keys"

	// Stateful primary: serves writes, then fails the read-back GET that follows.
	var wrote bool
	primary := handlerClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			wrote = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && wrote:
			http.Error(w, "read-back boom", http.StatusInternalServerError)
		default: // pre-write CurrentVersion probe → empty/absent
			http.Error(w, "nf", http.StatusNotFound)
		}
	})
	secondary := newFakeBao(t).client()

	err := DualWrite(ctx, primary, secondary, path, map[string]string{"k": "v"})
	if err == nil || !strings.Contains(err.Error(), "primary read-back failed") {
		t.Fatalf("want primary read-back failure, got %v", err)
	}
}

// TestDualWriteSecondaryReadBackFails covers the secondary read-back branch:
// both writes succeed, but the secondary 500s on its post-write hash read.
func TestDualWriteSecondaryReadBackFails(t *testing.T) {
	ctx := context.Background()
	const path = "secret/app/keys"

	primary := newFakeBao(t).client()
	var wrote bool
	secondary := handlerClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			wrote = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && wrote:
			http.Error(w, "read-back boom", http.StatusInternalServerError)
		default:
			http.Error(w, "nf", http.StatusNotFound)
		}
	})

	err := DualWrite(ctx, primary, secondary, path, map[string]string{"k": "v"})
	if err == nil || !strings.Contains(err.Error(), "secondary read-back failed") {
		t.Fatalf("want secondary read-back failure, got %v", err)
	}
}

// TestDualWriteRollbackFails covers the double-fault MANUAL-INTERVENTION branch:
// the secondary write fails, then the primary's rollback (priorVersion>0) also
// fails — here because the prior-version read returns unparseable data.
func TestDualWriteRollbackFails(t *testing.T) {
	ctx := context.Background()
	const path = "secret/app/keys"

	primary := handlerClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("version") != "":
			_, _ = w.Write([]byte("{not json")) // rollback's prior-version read → decode error
		case r.Method == http.MethodGet:
			// CurrentVersion probe → report an existing v1 so rollback takes the >0 path.
			_, _ = w.Write([]byte(`{"data":{"data":{"k":"v1"},"metadata":{"version":1}}}`))
		default: // POST writes succeed
			w.WriteHeader(http.StatusOK)
		}
	})
	secondary := newFakeBao(t)
	secondary.failWrite = true

	err := DualWrite(ctx, primary, secondary.client(), path, map[string]string{"k": "v2"})
	if err == nil || !strings.Contains(err.Error(), "MANUAL INTERVENTION") {
		t.Fatalf("want manual-intervention error, got %v", err)
	}
}
