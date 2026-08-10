package openbao

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if err := RunTeamLogin(TeamLoginOpts{}); err == nil {
		t.Error("login without --team must error")
	}
}

// TestKeycloakIssuerForLogin covers the three ways the issuer is resolved. The
// managed branch is the one worth pinning: it decides WHICH Keycloak an operator's
// device login is sent at, and its safety property — never fall back to a spec
// value on managed — is a deliberate choice that a refactor could quietly undo.
func TestKeycloakIssuerForLogin(t *testing.T) {
	const managedIssuer = "https://keycloak.lke635371.akamai-apl.net/realms/otomi"

	// clusterDef already emits its own bootstrap:, so spell the env out here — the
	// bootstrap block is exactly what these cases vary.
	env := func(bootstrap string) string {
		return `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: prod }
spec:
  cluster:
    clusterLabel: inst-prod
    region: us-ord
    objectStorage: { cluster: us-ord-1 }
    bootstrap: { name: inst-prod, ` + bootstrap + ` }
`
	}

	// Managed App Platform: no domainSuffix in the spec (Linode owns the domain), so
	// the issuer comes from apl-core's own otomi/otomi-api SSO_ISSUER. This is what
	// removes the hand-typed --issuer every managed login used to need.
	t.Run("managed discovers from the cluster", func(t *testing.T) {
		chdirTempDir(t)
		writeSpecInstance(t, map[string]string{
			"prod": env("managedAppPlatform: true"),
		})
		// The discovery call is a SEAM now, not a direct call into
		// internal/identityconfig — that import was a cycle in the test build. The
		// stub replaces what identityconfig.DiscoverIssuerFromCluster used to do,
		// and this test failing when the seam was introduced is the seam working:
		// it proved the path really did depend on that lookup.
		withIssuerDiscovery(t, func() string { return managedIssuer })
		got, err := keycloakIssuerForLogin("prod")
		if err != nil || got != managedIssuer {
			t.Fatalf("managed issuer = (%q, %v), want (%q, nil)", got, err, managedIssuer)
		}
	})

	// Discovery unavailable (kubeconfig not pointed at the cluster) must be a clear
	// error, NOT a fall back to some spec-derived guess — sending a device login at
	// the wrong Keycloak is the failure this guards.
	t.Run("managed with no cluster reach errors", func(t *testing.T) {
		chdirTempDir(t)
		writeSpecInstance(t, map[string]string{
			"prod": env("managedAppPlatform: true"),
		})
		withKubectl(t, func(string) ([]byte, error) { return nil, fmt.Errorf("no cluster") })
		got, err := keycloakIssuerForLogin("prod")
		if err == nil {
			t.Fatalf("undiscoverable issuer must error, got %q", got)
		}
		if got != "" {
			t.Errorf("no issuer may be returned alongside the error, got %q", got)
		}
		for _, want := range []string{"managedAppPlatform", "kubeconfig", "--issuer"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should mention %q — it is what the operator acts on", err, want)
			}
		}
	})

	// Self-install is untouched: derived from the spec, no cluster read at all.
	t.Run("domainSuffix path takes no cluster read", func(t *testing.T) {
		chdirTempDir(t)
		writeSpecInstance(t, map[string]string{
			"prod": env("domainSuffix: web.example.com"),
		})
		withKubectl(t, func(args string) ([]byte, error) {
			t.Errorf("the domainSuffix path must not read the cluster (kubectl %q)", args)
			return nil, fmt.Errorf("unreachable")
		})
		got, err := keycloakIssuerForLogin("prod")
		if err != nil || got != "https://keycloak.web.example.com/realms/otomi" {
			t.Fatalf("self-install issuer = (%q, %v)", got, err)
		}
	})
}
