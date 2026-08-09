package database

// Regression gate for the reason `llz ci assert-database` failed its first
// release-e2e: it built its OpenBao client with openbaoClient (env address, no
// fallback) while the bootstrap workflow that runs it sets no
// OPENBAO_ADDR_ACTIVE — OpenBao has no external ingress, so every CI-side caller
// reaches it by kubectl exec or by ephemeral port-forward. The gate died in 70ms
// on "OPENBAO_ADDR_ACTIVE is not set" and reded the whole run for want of an
// address rather than for anything about a
//
// The unit tests around this file all seam ListClusters/ReadCreds wholesale,
// so they exercised the verdict logic and never the connection — which is how
// the defect reached a cluster with a color.Green package. These tests call the
// DEFAULT implementations against a stub port-forward, so a revert to
// openbaoClient fails here instead of 40 minutes into an e2e.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
)

// baoStub is a TLS test server speaking the two KV endpoints this gate uses. The
// loopback HTTP client skips verification, exactly as it does against a real
// port-forward.
func baoStub(t *testing.T, keys []string, fields map[string]string) *httptest.Server {
	t.Helper()
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "LIST":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": keys}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": fields}})
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestListDBClusters_AutoForwardsWhenAddrUnset(t *testing.T) {
	clearOpenbaoEnv(t)
	// Exactly the bootstrap job's environment at this step: no address, and the
	// root token still un-revoked.
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	srv := baoStub(t, []string{"pg-e2e"}, nil)
	called := seamForward(t, srv.URL, nil)
	t.Cleanup(CloseBao)

	names, err := ListClusters(context.Background())
	if err != nil {
		t.Fatalf("ListClusters with OPENBAO_ADDR_ACTIVE unset = %v, want the port-forward fallback to carry it", err)
	}
	if !*called {
		t.Error("no port-forward was opened — the gate is using the env-only constructor again")
	}
	if len(names) != 1 || names[0] != "pg-e2e" {
		t.Errorf("declared clusters = %v, want [pg-e2e]", names)
	}
}

func TestReadDBCreds_AutoForwardsWhenAddrUnset(t *testing.T) {
	clearOpenbaoEnv(t)
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	srv := baoStub(t, nil, map[string]string{
		"endpoint": "pg.example", "port": "5432",
		"username": "linpostgres", "password": "hunter2",
	})
	called := seamForward(t, srv.URL, nil)
	t.Cleanup(CloseBao)

	creds, err := ReadCreds(context.Background(), "pg-e2e")
	if err != nil {
		t.Fatalf("ReadCreds with OPENBAO_ADDR_ACTIVE unset = %v, want the port-forward fallback to carry it", err)
	}
	if !*called {
		t.Error("no port-forward was opened — the gate is using the env-only constructor again")
	}
	if missing := creds.missingFields(); len(missing) > 0 {
		t.Errorf("credential record missing %v, want it fully read back", missing)
	}
}

func TestListDBClusters_ForwardFailureIsAnError(t *testing.T) {
	clearOpenbaoEnv(t)
	// No token at all: openbaoClientForward refuses before forwarding.
	seamForward(t, "https://127.0.0.1:1", nil)
	t.Cleanup(CloseBao)

	if _, err := ListClusters(context.Background()); err == nil {
		t.Fatal("ListClusters with no token = nil error, want a refusal")
	} else if strings.Contains(err.Error(), "OPENBAO_ADDR_ACTIVE is not set") {
		t.Errorf("error still blames the unset address (%v) — the env-only path is back", err)
	}
}

// The settle loop re-reads every cluster on every attempt. One port-forward per
// read would open, warm up and tear down a kubectl subprocess up to
// (settle/interval)×clusters times, so the connection is shared.
func TestDBBaoClient_OpensOnePortForwardForTheWholeRun(t *testing.T) {
	clearOpenbaoEnv(t)
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	srv := baoStub(t, []string{"pg-e2e"}, map[string]string{
		"endpoint": "pg.example", "port": "5432",
		"username": "linpostgres", "password": "hunter2",
	})

	opens := 0
	orig := OpenBaoForward
	OpenBaoForward = func(string) (*openbao.Client, func(), error) {
		opens++
		return openbao.NewWithClient(srv.URL, os.Getenv("OPENBAO_ROOT_TOKEN"), "", srv.Client()), func() {}, nil
	}
	t.Cleanup(func() { OpenBaoForward = orig; CloseBao() })

	ctx := context.Background()
	if _, err := ListClusters(ctx); err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := ReadCreds(ctx, "pg-e2e"); err != nil {
			t.Fatalf("ReadCreds: %v", err)
		}
	}
	if opens != 1 {
		t.Errorf("opened %d port-forwards across 4 reads, want 1", opens)
	}
}

// CloseBao must actually run the teardown and reset, or the kubectl
// port-forward outlives the process that opened it.
// CloseBao must actually run the teardown and reset, or the kubectl
// port-forward outlives the process that opened it.
// CloseBao must actually run the teardown and reset, or the kubectl
// port-forward outlives the process that opened it.
// CloseBao must actually run the teardown and reset, or the kubectl
// port-forward outlives the process that opened it.
func TestCloseDBBao_TearsDownAndResets(t *testing.T) {
	clearOpenbaoEnv(t)
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")

	torn := false
	orig := OpenBaoForward
	OpenBaoForward = func(string) (*openbao.Client, func(), error) {
		return openbao.New("https://127.0.0.1:1", "s.root", "", 10*time.Second), func() { torn = true }, nil
	}
	t.Cleanup(func() { OpenBaoForward = orig; CloseBao() })

	if _, err := dbBaoClient(); err != nil {
		t.Fatalf("dbBaoClient: %v", err)
	}
	CloseBao()
	if !torn {
		t.Error("CloseBao did not run the port-forward teardown")
	}
	if dbBao.opened || dbBao.client != nil {
		t.Error("CloseBao left the connection cached — a second run would reuse a dead tunnel")
	}
}

// baoStub is a COPY — fixtures travel by copy, not by export.
