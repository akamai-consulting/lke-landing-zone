package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func robotB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func robotSecretJSON(user, pass, host string) []byte {
	obj := map[string]any{"data": map[string]string{
		"username": robotB64(user), "password": robotB64(pass), "registry_host": robotB64(host),
	}}
	b, _ := json.Marshal(obj)
	return b
}

func TestDecodeRobotSecret(t *testing.T) {
	c, err := decodeRobotSecret(robotSecretJSON("robot$ci", "s3cret", "harbor.example.com"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Username != "robot$ci" || c.Password != "s3cret" || c.RegistryHost != "harbor.example.com" {
		t.Errorf("unexpected creds %+v", c)
	}

	// THE regression: "harbor." is NON-EMPTY, so every `== ""` guard passes it,
	// including the systeminfo fallback that was supposed to rescue this case.
	_, err = decodeRobotSecret(robotSecretJSON("robot$ci", "s3cret", "harbor."))
	if err == nil {
		t.Fatal(`registry_host "harbor." must be rejected — it is non-empty and every push and pull 401s`)
	}
	if !strings.Contains(err.Error(), "truncation") {
		t.Errorf("the failure should name the truncation class, got %q", err)
	}

	// Missing keys mean ESO has not materialized the Secret — a distinct failure
	// from a bad host, and it must not be read as an empty credential.
	partial, _ := json.Marshal(map[string]any{"data": map[string]string{"username": robotB64("x")}})
	if _, err := decodeRobotSecret(partial); err == nil {
		t.Error("a Secret missing password/registry_host must fail")
	}
	if _, err := decodeRobotSecret([]byte(`nope`)); err == nil {
		t.Error("an unparseable Secret must fail")
	}
	bad, _ := json.Marshal(map[string]any{"data": map[string]string{
		"username": "!!!not base64!!!", "password": robotB64("p"), "registry_host": robotB64("h.example.com")}})
	if _, err := decodeRobotSecret(bad); err == nil {
		t.Error("non-base64 Secret data must fail")
	}
}

func TestParseBearerChallenge(t *testing.T) {
	c, err := parseBearerChallenge(`Bearer realm="https://h.example.com/service/token",service="harbor-registry"`)
	if err != nil || c.Realm != "https://h.example.com/service/token" || c.Service != "harbor-registry" {
		t.Fatalf("unexpected (%+v,%v)", c, err)
	}
	if _, err := parseBearerChallenge(`Basic realm="x"`); err == nil {
		t.Error("a non-Bearer challenge must fail")
	}
	if _, err := parseBearerChallenge(`Bearer service="x"`); err == nil {
		t.Error("a challenge with no realm must fail — there is nowhere to get a token")
	}
}

func TestParseTokenResponseAndGrantedActions(t *testing.T) {
	raw := []byte(`{"token":"abc","access":[{"type":"repository","name":"library/x","actions":["pull","push"]}]}`)
	tok, access, err := parseTokenResponse(raw)
	if err != nil || tok != "abc" {
		t.Fatalf("unexpected (%q,%v)", tok, err)
	}
	g := grantedActions(access, "library/x")
	if !g["pull"] || !g["push"] {
		t.Errorf("expected pull+push, got %v", g)
	}
	// Access for a DIFFERENT repo must not count.
	if len(grantedActions(access, "library/other")) != 0 {
		t.Error("access scoped to another repository must not be read as a grant")
	}
	if _, _, err := parseTokenResponse([]byte(`{"access":[]}`)); err == nil {
		t.Error("a response with no token must fail")
	}
	if got := missingActions(map[string]bool{"pull": true}, "pull", "push"); len(got) != 1 || got[0] != "push" {
		t.Errorf("missingActions should report exactly the absent action, got %v", got)
	}
}

// fakeOCIRegistry stands up a distribution-v2 endpoint plus a token service, so the
// whole handshake runs for real over HTTP.
type fakeOCIRegistry struct {
	srv *httptest.Server
	// grant is the access list the token service returns.
	grant []tokenAccess
	// rejectAuth makes the token service 401 the basic auth.
	rejectAuth bool
	// denyUpload makes the blob-upload POST 403 despite a granted token.
	denyUpload bool
	sawUpload  bool
	sawCancel  bool
}

func newFakeOCIRegistry(t *testing.T) *fakeOCIRegistry {
	f := &fakeOCIRegistry{grant: []tokenAccess{{Type: "repository", Name: harborProbeRepo, Actions: []string{"pull", "push"}}}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.Header().Set("Www-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/service/token",service="harbor-registry"`, f.srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			w.WriteHeader(http.StatusNotFound) // NAME_UNKNOWN: token accepted, repo absent
		case strings.HasSuffix(r.URL.Path, "/blobs/uploads/") && r.Method == http.MethodPost:
			f.sawUpload = true
			if f.denyUpload {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Location", "/v2/"+harborProbeRepo+"/blobs/uploads/abc123")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete:
			f.sawCancel = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	mux.HandleFunc("/service/token", func(w http.ResponseWriter, r *http.Request) {
		if f.rejectAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok", "access": f.grant})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// seamHarborHTTP rewrites https://<host> to the test server, so probeHarborRoundTrip
// runs its real logic against the fake registry.
func seamHarborHTTP(t *testing.T, f *fakeOCIRegistry) {
	orig := harborHTTP
	t.Cleanup(func() { harborHTTP = orig })
	base, _ := url.Parse(f.srv.URL)
	harborHTTP = func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme, req.URL.Host = base.Scheme, base.Host
		return (&http.Client{Timeout: 5 * time.Second}).Do(req)
	}
}

func TestProbeHarborRoundTripHappyPath(t *testing.T) {
	f := newFakeOCIRegistry(t)
	seamHarborHTTP(t, f)
	creds := harborRobotCreds{Username: "robot$ci", Password: "p", RegistryHost: "harbor.example.com"}
	if err := probeHarborRoundTrip(creds, harborProbeRepo); err != nil {
		t.Fatalf("expected the round trip to succeed, got %v", err)
	}
	if !f.sawUpload {
		t.Error("the push half must actually open a blob upload session")
	}
	if !f.sawCancel {
		t.Error("the upload session must be cancelled so nothing is left behind")
	}
}

// THE sharp case: Harbor returns 200 with a VALID token and an EMPTY access list
// when the robot lacks the scope. A gate that stopped at the status code would
// pass for a robot that can do nothing.
func TestProbeHarborRoundTripRejectsTokenWithoutAccess(t *testing.T) {
	f := newFakeOCIRegistry(t)
	f.grant = nil
	seamHarborHTTP(t, f)
	err := probeHarborRoundTrip(harborRobotCreds{Username: "robot$ci", Password: "p", RegistryHost: "h.example.com"}, harborProbeRepo)
	if err == nil {
		t.Fatal("a 200 token carrying no access must NOT count as authorization")
	}
	if !strings.Contains(err.Error(), "not authorization") {
		t.Errorf("the failure should say a 200 is not authorization, got %v", err)
	}
	if f.sawUpload {
		t.Error("the push probe must not run once the token is known not to grant push")
	}
}

// A pull-only robot must fail the push half rather than quietly passing.
func TestProbeHarborRoundTripRequiresBothActions(t *testing.T) {
	f := newFakeOCIRegistry(t)
	f.grant = []tokenAccess{{Type: "repository", Name: harborProbeRepo, Actions: []string{"pull"}}}
	seamHarborHTTP(t, f)
	err := probeHarborRoundTrip(harborRobotCreds{Username: "r", Password: "p", RegistryHost: "h.example.com"}, harborProbeRepo)
	if err == nil || !strings.Contains(err.Error(), "push") {
		t.Errorf("a pull-only token must fail naming push, got %v", err)
	}
}

func TestProbeHarborRoundTripRejectedCredential(t *testing.T) {
	f := newFakeOCIRegistry(t)
	f.rejectAuth = true
	seamHarborHTTP(t, f)
	err := probeHarborRoundTrip(harborRobotCreds{Username: "robot$ci", Password: "wrong", RegistryHost: "h.example.com"}, harborProbeRepo)
	if err == nil || !strings.Contains(err.Error(), "REJECTED") {
		t.Errorf("a 401 from the token service must be reported as a rejected credential, got %v", err)
	}
}

// The registry and the token service can disagree: a token that grants push, and
// a registry that still refuses the upload. That is a real Harbor state and it
// must fail loudly rather than be masked by the token check having passed.
func TestProbeHarborRoundTripPushDeniedDespiteToken(t *testing.T) {
	f := newFakeOCIRegistry(t)
	f.denyUpload = true
	seamHarborHTTP(t, f)
	err := probeHarborRoundTrip(harborRobotCreds{Username: "r", Password: "p", RegistryHost: "h.example.com"}, harborProbeRepo)
	if err == nil || !strings.Contains(err.Error(), "PUSH DENIED") {
		t.Errorf("an upload refused despite a push-granting token must fail, got %v", err)
	}
}

// A registry serving /v2/ unauthenticated is its own finding, not a pass.
func TestProbeHarborRoundTripUnauthenticatedRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	orig := harborHTTP
	t.Cleanup(func() { harborHTTP = orig })
	base, _ := url.Parse(srv.URL)
	harborHTTP = func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme, req.URL.Host = base.Scheme, base.Host
		return (&http.Client{Timeout: 5 * time.Second}).Do(req)
	}
	err := probeHarborRoundTrip(harborRobotCreds{Username: "r", Password: "p", RegistryHost: "h.example.com"}, harborProbeRepo)
	if err == nil || !strings.Contains(err.Error(), "unauthenticated") {
		t.Errorf("an unauthenticated registry must be reported, got %v", err)
	}
}

// seamHarborCluster stubs the two cluster reads: whether the component's
// namespace exists, and the Secret inside it.
func seamHarborCluster(t *testing.T, nsPresent bool, nsErr error, secret []byte, secretErr error) {
	t.Helper()
	oN, oS := namespaceExists, readHarborRobotSecret
	t.Cleanup(func() { namespaceExists, readHarborRobotSecret = oN, oS })
	namespaceExists = func(string) (bool, error) { return nsPresent, nsErr }
	readHarborRobotSecret = func(string, string) ([]byte, error) { return secret, secretErr }
}

// An absent credential Secret INSIDE A PRESENT NAMESPACE must fail with the
// ESO-shaped diagnosis, not be skipped: a gate that skips when the credential is
// missing cannot catch a credential that stopped being delivered.
func TestRunAssertHarborRoundTripMissingSecretFails(t *testing.T) {
	seamHarborCluster(t, true, nil, nil, nil) // namespace there, Secret absent
	err := runCIAssertHarborRoundTrip("ns", "name", "", harborProbeRepo, 0, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "secret/harbor/robot") {
		t.Errorf("a missing robot Secret must fail naming the OpenBao path, got %v", err)
	}
}

// A cluster that never deployed llz-cert-automation has no namespace and no
// robot credential to round-trip. Managed App Platform renders a minimal app set
// and omits it; on lke638103 the namespace genuinely did not exist. Failing there
// reds a correct cluster for a component it was never asked to run.
func TestRunAssertHarborRoundTripSkipsWhenComponentAbsent(t *testing.T) {
	seamHarborCluster(t, false, nil, nil, fmt.Errorf("must not be read"))
	if err := runCIAssertHarborRoundTrip("ns", "name", "", harborProbeRepo, 0, time.Millisecond); err != nil {
		t.Errorf("an undeployed component must SKIP, not fail: %v", err)
	}
}

// "Could not tell" is not "not deployed". An unreadable cluster must fail rather
// than skip, or a broken kubeconfig silently turns this gate off.
func TestRunAssertHarborRoundTripFailsWhenNamespaceUnreadable(t *testing.T) {
	seamHarborCluster(t, false, fmt.Errorf("connection refused"), nil, nil)
	if err := runCIAssertHarborRoundTrip("ns", "name", "", harborProbeRepo, 0, time.Millisecond); err == nil {
		t.Error("an unreadable namespace check must fail, not degrade to a skip")
	}
}

// The Secret is written by ESO from a path the harbor-robot-provisioner CronJob
// seeds only after Harbor is serving, so a fresh cluster has a window where it is
// legitimately absent. The read must be RETRIED inside the settle budget rather
// than decided once, which is what made this lane fail on the documented window.
func TestRunAssertHarborRoundTripRetriesTheSecretRead(t *testing.T) {
	oN, oS := namespaceExists, readHarborRobotSecret
	t.Cleanup(func() { namespaceExists, readHarborRobotSecret = oN, oS })
	namespaceExists = func(string) (bool, error) { return true, nil }
	reads := 0
	readHarborRobotSecret = func(string, string) ([]byte, error) {
		reads++
		return nil, nil // always absent
	}
	_ = runCIAssertHarborRoundTrip("ns", "name", "", harborProbeRepo, 50*time.Millisecond, 10*time.Millisecond)
	if reads < 2 {
		t.Errorf("the Secret was read %d time(s) — absence must be retried within the settle budget, "+
			"or the gate decides during the window ESO is documented to need", reads)
	}
}
