package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDiscoverOIDC(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/realms/otomi/.well-known/openid-configuration" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"device_authorization_endpoint": "https://kc/dev",
			"token_endpoint":                "https://kc/token",
		})
	}))
	defer srv.Close()

	cfg, err := discoverOIDC(srv.Client(), srv.URL+"/realms/otomi")
	if err != nil {
		t.Fatalf("discoverOIDC: %v", err)
	}
	if cfg.DeviceEndpoint != "https://kc/dev" || cfg.TokenEndpoint != "https://kc/token" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestDiscoverOIDC_MissingEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token_endpoint": "https://kc/token"}) // no device endpoint
	}))
	defer srv.Close()
	if _, err := discoverOIDC(srv.Client(), srv.URL); err == nil {
		t.Error("missing device_authorization_endpoint must error (device flow disabled)")
	}
}

func TestStartDeviceGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != "llz" || r.Form.Get("scope") != "openid" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(deviceGrant{
			DeviceCode: "dc", UserCode: "ABCD-EFGH",
			VerificationURIComplete: "https://kc/device?user_code=ABCD-EFGH", Interval: 0, ExpiresIn: 600,
		})
	}))
	defer srv.Close()

	g, err := startDeviceGrant(srv.Client(), srv.URL, "llz")
	if err != nil {
		t.Fatalf("startDeviceGrant: %v", err)
	}
	if g.DeviceCode != "dc" || g.UserCode != "ABCD-EFGH" {
		t.Errorf("grant = %+v", g)
	}
	if g.Interval != 5 { // 0 → RFC 8628 default
		t.Errorf("interval defaulted to %d, want 5", g.Interval)
	}
}

// pollDeviceToken must survive authorization_pending + slow_down before the user
// finishes, then return the id_token — without ever sleeping in the test.
func TestPollDeviceToken_PendingThenSuccess(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("device_code") != "dc" {
			t.Errorf("unexpected token poll form: %v", r.Form)
		}
		n++
		switch n {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
		case 2:
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "slow_down"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"id_token": "id.jwt.token"})
		}
	}))
	defer srv.Close()

	var slept int
	tok, err := pollDeviceToken(srv.Client(), srv.URL, "llz", "dc", 5, func(time.Duration) { slept++ }, 10)
	if err != nil {
		t.Fatalf("pollDeviceToken: %v", err)
	}
	if tok != "id.jwt.token" {
		t.Errorf("token = %q", tok)
	}
	if n != 3 || slept != 2 {
		t.Errorf("polls=%d sleeps=%d, want 3 polls / 2 sleeps (pending + slow_down)", n, slept)
	}
}

func TestPollDeviceToken_HardError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied", "error_description": "user declined"})
	}))
	defer srv.Close()
	if _, err := pollDeviceToken(srv.Client(), srv.URL, "llz", "dc", 1, func(time.Duration) {}, 5); err == nil {
		t.Error("access_denied must return an error, not keep polling")
	}
}

func TestPollDeviceToken_TimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
	}))
	defer srv.Close()
	if _, err := pollDeviceToken(srv.Client(), srv.URL, "llz", "dc", 1, func(time.Duration) {}, 3); err == nil {
		t.Error("never-completing login must time out after maxPolls")
	}
}

// flakyRT fails the first n round-trips with a network-style error, then delegates
// — to prove a transient blip during the browser wait doesn't kill the login.
type flakyRT struct {
	fails int
	base  http.RoundTripper
}

func (f *flakyRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.fails > 0 {
		f.fails--
		return nil, fmt.Errorf("dial tcp: connection refused")
	}
	return f.base.RoundTrip(r)
}

func TestPollDeviceToken_RetriesTransientError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": "idtok"})
	}))
	defer srv.Close()
	hc := &http.Client{Transport: &flakyRT{fails: 2, base: http.DefaultTransport}}
	tok, err := pollDeviceToken(hc, srv.URL, "llz", "dc", 1, func(time.Duration) {}, 10)
	if err != nil || tok != "idtok" {
		t.Fatalf("transient poll errors must be retried; got (%q, %v)", tok, err)
	}
}

func TestRunOpenbaoLogin_RequiresTeam(t *testing.T) {
	if err := runOpenbaoLogin(openbaoLoginOpts{}); err == nil {
		t.Error("login without --team must error")
	}
}
