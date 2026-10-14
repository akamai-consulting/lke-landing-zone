package openbao

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func introspectServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return NewWithClient(s.URL, "tok", "", s.Client())
}

func TestCapabilitiesSelf(t *testing.T) {
	c := introspectServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sys/capabilities-self" || r.Method != http.MethodPost || r.Header.Get("X-Vault-Token") != "tok" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"a/b":["create","update"],"c":["deny"],"capabilities":["deny"]}}`))
	})
	got, err := c.CapabilitiesSelf(context.Background(), []string{"a/b", "c"})
	if err != nil || strings.Join(got["a/b"], ",") != "create,update" || strings.Join(got["c"], ",") != "deny" {
		t.Fatalf("got %v, %v", got, err)
	}
	// Fail closed: an answer that omits a requested path is an error, not "no caps".
	if _, err := c.CapabilitiesSelf(context.Background(), []string{"a/b", "missing"}); err == nil {
		t.Fatal("omitted path must be an error")
	}
	if _, err := c.CapabilitiesSelf(context.Background(), nil); err == nil {
		t.Fatal("empty request must be an error")
	}
}

func TestCapabilitiesSelfMalformedAndDenied(t *testing.T) {
	bad := introspectServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":{"p":"nope"}}`)) })
	if _, err := bad.CapabilitiesSelf(context.Background(), []string{"p"}); err == nil {
		t.Fatal("non-list capabilities must be an error")
	}
	denied := introspectServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if _, err := denied.CapabilitiesSelf(context.Background(), []string{"p"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("403 must surface as an error carrying the status, got %v", err)
	}
}

func TestMountType(t *testing.T) {
	c := introspectServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sys/internal/ui/mounts/pki-x":
			_, _ = w.Write([]byte(`{"data":{"type":"pki","path":"pki-x/"}}`))
		case "/v1/sys/internal/ui/mounts/pki-x/child":
			// Longest-prefix resolution: the parent answers for a child path.
			_, _ = w.Write([]byte(`{"data":{"type":"pki","path":"pki-x/"}}`))
		case "/v1/sys/internal/ui/mounts/empty":
			_, _ = w.Write([]byte(`{"data":{}}`))
		case "/v1/sys/internal/ui/mounts/forbidden":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()
	if ty, ok, err := c.MountType(ctx, "pki-x"); err != nil || !ok || ty != "pki" {
		t.Fatalf("pki-x: %q %v %v", ty, ok, err)
	}
	if _, ok, err := c.MountType(ctx, "pki-x/child"); err != nil || ok {
		t.Fatalf("a parent mount must not answer for a child: ok=%v err=%v", ok, err)
	}
	if _, ok, err := c.MountType(ctx, "absent"); err != nil || ok {
		t.Fatalf("absent: ok=%v err=%v", ok, err)
	}
	if _, _, err := c.MountType(ctx, "empty"); err == nil {
		t.Fatal("an answer with no type must be an error")
	}
	if _, _, err := c.MountType(ctx, "forbidden"); err == nil {
		t.Fatal("403 must be an error")
	}
}

func TestRead(t *testing.T) {
	c := introspectServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/m/roles/x" {
			_, _ = w.Write([]byte(`{"data":{"client_flag":true}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	d, ok, err := c.Read(context.Background(), "m/roles/x")
	if err != nil || !ok || d["client_flag"] != true {
		t.Fatalf("got %v %v %v", d, ok, err)
	}
	if _, ok, err := c.Read(context.Background(), "m/roles/none"); err != nil || ok {
		t.Fatalf("404 → ok=false, got ok=%v err=%v", ok, err)
	}
}

func TestTokenPolicies(t *testing.T) {
	c := introspectServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/token/lookup-self" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"policies":["default","p1"]}}`))
	})
	got, err := c.TokenPolicies(context.Background())
	if err != nil || strings.Join(got, ",") != "default,p1" {
		t.Fatalf("got %v, %v", got, err)
	}
	// Fail closed: no policies list, a non-2xx and a malformed body are errors.
	for name, h := range map[string]http.HandlerFunc{
		"no list":   func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":{}}`)) },
		"403":       func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		"malformed": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{`)) },
	} {
		if _, err := introspectServer(t, h).TokenPolicies(context.Background()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestList(t *testing.T) {
	c := introspectServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "LIST" || r.URL.Path != "/v1/pki-x/issuers" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"keys":["a1","b2"]}}`))
	})
	got, err := c.List(context.Background(), "pki-x/issuers")
	if err != nil || strings.Join(got, ",") != "a1,b2" {
		t.Fatalf("got %v, %v", got, err)
	}
	empty := introspectServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if got, err := empty.List(context.Background(), "p"); err != nil || len(got) != 0 {
		t.Fatalf("404 is an empty list: %v, %v", got, err)
	}
	for name, h := range map[string]http.HandlerFunc{
		"403":       func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
		"malformed": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{`)) },
	} {
		if _, err := introspectServer(t, h).List(context.Background(), "p"); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
