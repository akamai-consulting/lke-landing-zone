package openbao

// Edge coverage for the HTTP wrappers' success/failure boundary and for the two
// Rollback error branches, all of which mutation testing showed were unasserted.
//
// Every wrapper here decides success with `resp.StatusCode < 200 ||
// resp.StatusCode >= 300`, but the suite only ever exercised them at 200 and at
// an obvious error status (403/404/500). Nothing pinned the UPPER edge: relaxing
// `>= 300` to `> 300` — one character — makes an HTTP 300 read as success, and
// every test still passed. 300 is not hypothetical for this client: OpenBao sits
// behind an in-cluster Service and a redirect-shaped response admitted as success
// would be decoded as a secret (readKV), as a seal state (SealStatus), or as
// "the write landed" (Write).
//
// So each wrapper is driven at 200 / 299 / 300 with a body that DECODES CLEANLY.
// The clean body is load-bearing: if the 300 payload were garbage, a wrapper that
// wrongly admitted the status would still fail on the parse and the test would
// prove nothing about the boundary.
//
// 199 (the lower edge) is deliberately absent: Go's net/http server treats a 1xx
// WriteHeader as an informational header and keeps the response open, so a 199
// cannot be delivered as a final status through the httptest seam.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// baoWrapper is one call that treats 2xx as success and anything else as failure.
type baoWrapper struct {
	name string
	body string // decodes cleanly, so only the STATUS decides the outcome
	call func(context.Context, *httptest.Server) error
}

var baoWrappers = []baoWrapper{
	{
		name: "authLogin", // via KubernetesLogin; JWTLogin/OIDCLogin share the body
		body: `{"auth":{"client_token":"s.tok"}}`,
		call: func(ctx context.Context, srv *httptest.Server) error {
			_, err := KubernetesLogin(ctx, srv.Client(), srv.URL, "kubernetes", "role", "jwt")
			return err
		},
	},
	{
		name: "readKV", // via Get
		body: `{"data":{"data":{"k":"v"},"metadata":{"version":1}}}`,
		call: func(ctx context.Context, srv *httptest.Server) error {
			_, _, err := baoClient(srv).Get(ctx, "secret/app/x", "k")
			return err
		},
	},
	{
		name: "SealStatus",
		body: `{"sealed":false,"initialized":true}`,
		call: func(ctx context.Context, srv *httptest.Server) error {
			_, err := baoClient(srv).SealStatus(ctx)
			return err
		},
	},
	{
		name: "MetadataUpdatedTime",
		body: `{"data":{"updated_time":"2026-06-01T12:00:00Z"}}`,
		call: func(ctx context.Context, srv *httptest.Server) error {
			_, _, err := baoClient(srv).MetadataUpdatedTime(ctx, "secret/app/x")
			return err
		},
	},
	{
		name: "MetadataList",
		body: `{"data":{"keys":["a"]}}`,
		call: func(ctx context.Context, srv *httptest.Server) error {
			_, _, err := baoClient(srv).MetadataList(ctx, "secret/app/x")
			return err
		},
	},
	{
		name: "Write",
		body: `{}`,
		call: func(ctx context.Context, srv *httptest.Server) error {
			return baoClient(srv).Write(ctx, "secret/app/x", map[string]string{"k": "v"})
		},
	},
}

func baoClient(srv *httptest.Server) *Client {
	return New(srv.URL, "t", "", 5*time.Second)
}

func TestWrapperSuccessBoundaryIs2xx(t *testing.T) {
	edges := []struct {
		status  int
		wantErr bool
	}{
		{http.StatusOK, false},
		{299, false},
		{300, true}, // the first NON-success status: a redirect is not a result
	}
	for _, w := range baoWrappers {
		for _, e := range edges {
			t.Run(fmt.Sprintf("%s/%d", w.name, e.status), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
					rw.WriteHeader(e.status)
					_, _ = rw.Write([]byte(w.body))
				}))
				t.Cleanup(srv.Close)

				err := w.call(context.Background(), srv)
				if e.wantErr && err == nil {
					t.Errorf("HTTP %d was accepted as success — the 2xx boundary is wrong", e.status)
				}
				if !e.wantErr && err != nil {
					t.Errorf("HTTP %d should succeed, got %v", e.status, err)
				}
			})
		}
	}
}

// TestDoSetsContentTypeOnlyWithABody pins do()'s `if body != nil` header branch.
// Inverted, it stamps application/json on the bodyless GETs and omits it from the
// writes that actually carry JSON — OpenBao is lenient enough about both that no
// existing test noticed.
func TestDoSetsContentTypeOnlyWithABody(t *testing.T) {
	var getCT, postCT string
	var sawGet, sawPost bool
	c := handlerClient(t, "", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sawGet, getCT = true, r.Header.Get("Content-Type")
			http.Error(w, "nf", http.StatusNotFound)
		case http.MethodPost:
			sawPost, postCT = true, r.Header.Get("Content-Type")
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unhandled", http.StatusBadRequest)
		}
	})
	ctx := context.Background()

	if _, _, err := c.Get(ctx, "secret/app/x", "k"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := c.Write(ctx, "secret/app/x", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !sawGet || !sawPost {
		t.Fatalf("handler saw get=%v post=%v, want both", sawGet, sawPost)
	}
	if postCT != "application/json" {
		t.Errorf("POST Content-Type = %q, want application/json", postCT)
	}
	if getCT != "" {
		t.Errorf("bodyless GET Content-Type = %q, want it unset", getCT)
	}
}

// methodFailRT fails the given method at the TRANSPORT level — (nil, err), the
// shape a connection reset or DNS failure takes — and delegates everything else.
// This is the only seam that reaches Rollback's two error branches: a handler
// returning 5xx still yields a non-nil *http.Response, so it exercises neither.
type methodFailRT struct {
	method string
	base   http.RoundTripper
}

func (m methodFailRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == m.method {
		return nil, fmt.Errorf("simulated transport failure")
	}
	return m.base.RoundTrip(r)
}

func failingClient(t *testing.T, method string, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewWithClient(srv.URL, "t", "",
		&http.Client{Transport: methodFailRT{method: method, base: srv.Client().Transport}})
}

// TestRollbackDeleteTransportFailure covers the priorVersion==0 branch's
// `if err == nil { resp.Body.Close() }`. Inverted it closes the body of the
// response it does NOT have (resp is nil exactly when err is non-nil), so the
// best-effort delete panics instead of returning the error to DualWrite — which
// reads it to decide between "rolled back" and "MANUAL INTERVENTION".
func TestRollbackDeleteTransportFailure(t *testing.T) {
	c := failingClient(t, http.MethodDelete, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.Rollback(context.Background(), "secret/app/x", 0); err == nil {
		t.Fatal("Rollback(0) must return the DELETE's transport error, not nil")
	}
}

// TestRollbackRestoreTransportFailure covers the restore POST's `if err != nil`.
// Inverted, a failed restore returns nil — DualWrite then reports "primary rolled
// back to vN" for a rollback that never happened, so the primary silently keeps
// the value the secondary rejected.
func TestRollbackRestoreTransportFailure(t *testing.T) {
	c := failingClient(t, http.MethodPost, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"data":{"k":"v1"},"metadata":{"version":1}}}`))
	})
	if err := c.Rollback(context.Background(), "secret/app/x", 1); err == nil {
		t.Fatal("Rollback must return the restore POST's transport error, not nil")
	}
}
