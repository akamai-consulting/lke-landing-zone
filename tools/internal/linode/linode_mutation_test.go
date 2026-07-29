package linode

// linode_mutation_test.go pins the request-shaping half of Client.do. Every
// existing test asserts on the RESPONSE; the only thing checked on the way out
// is the Authorization header, so do's other conditional — Content-Type is set
// for a body-carrying request and omitted for a bodyless one — was untested in
// both directions. Sending `Content-Type: application/json` on a bodyless GET,
// or omitting it on a POST, is exactly the kind of thing an API rejects and a
// mock does not.

import (
	"context"
	"net/http"
	"testing"
)

func TestDoSetsContentTypeOnlyWithBody(t *testing.T) {
	ctx := context.Background()
	var gotMethod, gotContentType, gotAuth string
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotContentType, gotAuth = r.Method, r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{}, "pages": 1})
	})

	// Body-carrying verbs must declare JSON.
	if err := c.PutControlPlaneACL(ctx, 7, ControlPlaneACL{Enabled: true}); err != nil {
		t.Fatalf("PutControlPlaneACL = %v", err)
	}
	if gotContentType != "application/json" {
		t.Errorf("PUT with body: Content-Type = %q, want application/json", gotContentType)
	}
	if _, err := c.CreateProfileToken(ctx, "ci", "*", "2030-01-01T00:00:00"); err != nil {
		t.Fatalf("CreateProfileToken = %v", err)
	}
	if gotMethod != http.MethodPost || gotContentType != "application/json" {
		t.Errorf("POST with body: %s Content-Type = %q, want POST application/json", gotMethod, gotContentType)
	}

	// Bodyless verbs must not: there is no payload to describe.
	if _, err := c.ListProfileTokens(ctx); err != nil {
		t.Fatalf("ListProfileTokens = %v", err)
	}
	if gotMethod != http.MethodGet || gotContentType != "" {
		t.Errorf("bodyless GET: Content-Type = %q, want empty", gotContentType)
	}
	if err := c.DeleteProfileToken(ctx, 9); err != nil {
		t.Fatalf("DeleteProfileToken = %v", err)
	}
	if gotMethod != http.MethodDelete || gotContentType != "" {
		t.Errorf("bodyless DELETE: Content-Type = %q, want empty", gotContentType)
	}

	// Authorization is unconditional, on the bodyless path too.
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", gotAuth)
	}
}
