package openbao

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBao is a minimal in-memory KV v2 store for testing the dual-write logic.
type fakeBao struct {
	mu        sync.Mutex
	versions  map[string][]map[string]any // data-api path -> versions (idx 0 == v1)
	failWrite bool                        // POST returns 500
	failRead  bool                        // GET returns 500 (a transport/permission blip, NOT a 404)
	tamper    func(map[string]any) map[string]any
	srv       *httptest.Server
}

func newFakeBao(t *testing.T) *fakeBao {
	f := &fakeBao{versions: map[string][]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBao) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1/")

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "secret/data/"):
		if f.failRead {
			http.Error(w, "upstream unavailable", http.StatusInternalServerError)
			return
		}
		vers := f.versions[path]
		if len(vers) == 0 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		idx := len(vers) - 1
		if v := r.URL.Query().Get("version"); v != "" {
			// 1-based version selection
			switch v {
			case "1":
				idx = 0
			}
		}
		data := vers[idx]
		if f.tamper != nil {
			data = f.tamper(data)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"data": data, "metadata": map[string]any{"version": idx + 1}},
		}); err != nil {
			http.Error(w, "encode failed", http.StatusInternalServerError)
			return
		}
	case r.Method == http.MethodPost && strings.HasPrefix(path, "secret/data/"):
		if f.failWrite {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}
		f.versions[path] = append(f.versions[path], body.Data)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "secret/metadata/"):
		delete(f.versions, strings.Replace(path, "secret/metadata/", "secret/data/", 1))
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unhandled", http.StatusBadRequest)
	}
}

func (f *fakeBao) client() *Client { return New(f.srv.URL, "t", "", 5*time.Second) }

func (f *fakeBao) latest(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	vers := f.versions[DataPath(path)]
	if len(vers) == 0 {
		return nil
	}
	return vers[len(vers)-1]
}

func TestDualWriteHappyPath(t *testing.T) {
	p, s := newFakeBao(t), newFakeBao(t)
	ctx := context.Background()
	data := map[string]string{"api_key": "deadbeef", "enabled": "true"}
	if err := DualWrite(ctx, p.client(), s.client(), "secret/app/keys", data); err != nil {
		t.Fatalf("DualWrite: %v", err)
	}
	if got := p.latest("secret/app/keys")["api_key"]; got != "deadbeef" {
		t.Errorf("primary api_key = %v", got)
	}
	if got := s.latest("secret/app/keys")["enabled"]; got != "true" {
		t.Errorf("secondary enabled = %v", got)
	}
}

func TestDualWriteSecondaryFailsRollsBackPrimary(t *testing.T) {
	p, s := newFakeBao(t), newFakeBao(t)
	s.failWrite = true // secondary write 500s
	ctx := context.Background()

	err := DualWrite(ctx, p.client(), s.client(), "secret/app/keys", map[string]string{"k": "v"})
	if err == nil || !strings.Contains(err.Error(), "secondary write failed") {
		t.Fatalf("want secondary-write error, got %v", err)
	}
	// Prior version was 0 (no secret existed) → rollback deletes it entirely.
	if p.latest("secret/app/keys") != nil {
		t.Errorf("primary should have been rolled back (deleted), got %v", p.latest("secret/app/keys"))
	}
}

func TestDualWriteHashMismatch(t *testing.T) {
	p, s := newFakeBao(t), newFakeBao(t)
	// Secondary silently serves altered data on read-back → hashes differ.
	s.tamper = func(m map[string]any) map[string]any { return map[string]any{"k": "TAMPERED"} }
	ctx := context.Background()

	err := DualWrite(ctx, p.client(), s.client(), "secret/app/keys", map[string]string{"k": "v"})
	if err == nil || !strings.Contains(err.Error(), "HASH MISMATCH") {
		t.Fatalf("want hash-mismatch error, got %v", err)
	}
}

func TestGetMissingKey(t *testing.T) {
	p := newFakeBao(t)
	ctx := context.Background()
	if err := p.client().Write(ctx, "secret/app/keys", map[string]string{"present": "1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := p.client().Get(ctx, "secret/app/keys", "absent"); ok {
		t.Error("absent key reported present")
	}
	v, ok, err := p.client().Get(ctx, "secret/app/keys", "present")
	if err != nil || !ok || v != "1" {
		t.Errorf("Get present = %q, ok=%v, err=%v", v, ok, err)
	}
}

func TestPathHelpers(t *testing.T) {
	if DataPath("secret/app/x") != "secret/data/app/x" {
		t.Error("DataPath")
	}
	if MetadataPath("secret/app/x") != "secret/metadata/app/x" {
		t.Error("MetadataPath")
	}
	if ValidatePath("kv/app") == nil {
		t.Error("ValidatePath should reject non-secret/ path")
	}
}

// TestDualWriteRefusesWhenPriorVersionUnreadable is a DATA-LOSS regression test.
//
// DualWrite used to discard the error from CurrentVersion:
//
//	priorP, _ := primary.CurrentVersion(ctx, path)
//
// CurrentVersion returns (0, nil) when the secret genuinely does not exist and
// (0, err) when it could not be READ. Rollback treats priorVersion 0 as "nothing
// was here before" and DELETEs the metadata path — which in KV v2 destroys the
// secret and every version.
//
// So a read blip followed by a secondary write failure permanently destroyed a
// live credential that the rollback existed to restore.
func TestDualWriteRefusesWhenPriorVersionUnreadable(t *testing.T) {
	primary, secondary := newFakeBao(t), newFakeBao(t)
	pc, sc := primary.client(), secondary.client()
	ctx := context.Background()
	const path = "secret/linode/api-token"

	// A live secret with history — exactly what must not be destroyed.
	if err := pc.Write(ctx, path, map[string]string{"token": "v1"}); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	if err := pc.Write(ctx, path, map[string]string{"token": "v2"}); err != nil {
		t.Fatalf("seed v2: %v", err)
	}

	// The read blips, and the secondary write would fail. Previously: prior
	// version reads as 0 -> rollback DELETEs -> secret gone.
	primary.failRead = true
	secondary.failWrite = true

	err := DualWrite(ctx, pc, sc, path, map[string]string{"token": "v3"})
	if err == nil {
		t.Fatal("DualWrite must fail when it cannot read the prior version")
	}
	if !strings.Contains(err.Error(), "no change made") {
		t.Errorf("error should say nothing was changed, got: %v", err)
	}

	// THE ASSERTION THAT MATTERS: the secret survived.
	primary.failRead = false
	kv, ok, readErr := pc.readKV(ctx, path)
	if readErr != nil {
		t.Fatalf("re-read after refused DualWrite: %v", readErr)
	}
	if !ok {
		t.Fatal("the secret was DESTROYED — rollback deleted the metadata path because an unreadable prior version looked like 'no prior secret'")
	}
	if got := kv.Data.Data["token"]; got != "v2" {
		t.Errorf("token = %v, want the untouched v2", got)
	}
}
