package assertregistry

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/harborauth"
)

func robotB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func robotSecretJSON(user, pass, host string) []byte {
	obj := map[string]any{"data": map[string]string{
		"username": robotB64(user), "password": robotB64(pass), "registry_host": robotB64(host),
	}}
	b, _ := json.Marshal(obj)
	return b
}

// fakeOCIRegistry stands up a distribution-v2 endpoint plus a token service, so the
// whole handshake runs for real over HTTP.
type fakeOCIRegistry struct {
	srv *httptest.Server
	// grant is the access list the token service returns.
	grant []harborauth.TokenAccess
	// rejectAuth makes the token service 401 the basic auth.
	rejectAuth bool
	// denyUpload makes the blob-upload POST 403 despite a granted token.
	denyUpload bool
	sawUpload  bool
	sawCancel  bool
}

func newFakeOCIRegistry(t *testing.T) *fakeOCIRegistry {
	f := &fakeOCIRegistry{grant: []harborauth.TokenAccess{{Type: "repository", Name: ProbeRepo, Actions: []string{"pull", "push"}}}}
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
			w.Header().Set("Location", "/v2/"+ProbeRepo+"/blobs/uploads/abc123")
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
	orig := harborauth.HTTP
	t.Cleanup(func() { harborauth.HTTP = orig })
	base, _ := url.Parse(f.srv.URL)
	harborauth.HTTP = func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme, req.URL.Host = base.Scheme, base.Host
		return (&http.Client{Timeout: 5 * time.Second}).Do(req)
	}
}

func TestProbeHarborRoundTripHappyPath(t *testing.T) {
	f := newFakeOCIRegistry(t)
	seamHarborHTTP(t, f)
	creds := harborauth.RobotCreds{Username: "robot$ci", Password: "p", RegistryHost: "harbor.example.com"}
	if err := probeHarborRoundTrip(creds, ProbeRepo); err != nil {
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
	err := probeHarborRoundTrip(harborauth.RobotCreds{Username: "robot$ci", Password: "p", RegistryHost: "h.example.com"}, ProbeRepo)
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
	f.grant = []harborauth.TokenAccess{{Type: "repository", Name: ProbeRepo, Actions: []string{"pull"}}}
	seamHarborHTTP(t, f)
	err := probeHarborRoundTrip(harborauth.RobotCreds{Username: "r", Password: "p", RegistryHost: "h.example.com"}, ProbeRepo)
	if err == nil || !strings.Contains(err.Error(), "push") {
		t.Errorf("a pull-only token must fail naming push, got %v", err)
	}
}

func TestProbeHarborRoundTripRejectedCredential(t *testing.T) {
	f := newFakeOCIRegistry(t)
	f.rejectAuth = true
	seamHarborHTTP(t, f)
	err := probeHarborRoundTrip(harborauth.RobotCreds{Username: "robot$ci", Password: "wrong", RegistryHost: "h.example.com"}, ProbeRepo)
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
	err := probeHarborRoundTrip(harborauth.RobotCreds{Username: "r", Password: "p", RegistryHost: "h.example.com"}, ProbeRepo)
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
	orig := harborauth.HTTP
	t.Cleanup(func() { harborauth.HTTP = orig })
	base, _ := url.Parse(srv.URL)
	harborauth.HTTP = func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme, req.URL.Host = base.Scheme, base.Host
		return (&http.Client{Timeout: 5 * time.Second}).Do(req)
	}
	err := probeHarborRoundTrip(harborauth.RobotCreds{Username: "r", Password: "p", RegistryHost: "h.example.com"}, ProbeRepo)
	if err == nil || !strings.Contains(err.Error(), "unauthenticated") {
		t.Errorf("an unauthenticated registry must be reported, got %v", err)
	}
}

// seamHarborCluster stubs the two cluster reads: whether the component's
// namespace exists, and the Secret inside it.
func seamHarborCluster(t *testing.T, nsPresent bool, nsErr error, secret []byte, secretErr error) {
	t.Helper()
	oN, oS := harborauth.NamespaceExists, harborauth.ReadRobotSecret
	t.Cleanup(func() { harborauth.NamespaceExists, harborauth.ReadRobotSecret = oN, oS })
	harborauth.NamespaceExists = func(string) (bool, error) { return nsPresent, nsErr }
	harborauth.ReadRobotSecret = func(string, string) ([]byte, error) { return secret, secretErr }
}

// An absent credential Secret INSIDE A PRESENT NAMESPACE must fail with the
// ESO-shaped diagnosis, not be skipped: a gate that skips when the credential is
// missing cannot catch a credential that stopped being delivered.
func TestRunAssertHarborRoundTripMissingSecretFails(t *testing.T) {
	seamHarborCluster(t, true, nil, nil, nil) // namespace there, Secret absent
	err := Run("ns", "name", "", ProbeRepo, 0, time.Millisecond)
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
	if err := Run("ns", "name", "", ProbeRepo, 0, time.Millisecond); err != nil {
		t.Errorf("an undeployed component must SKIP, not fail: %v", err)
	}
}

// "Could not tell" is not "not deployed". An unreadable cluster must fail rather
// than skip, or a broken kubeconfig silently turns this gate off.
func TestRunAssertHarborRoundTripFailsWhenNamespaceUnreadable(t *testing.T) {
	seamHarborCluster(t, false, fmt.Errorf("connection refused"), nil, nil)
	if err := Run("ns", "name", "", ProbeRepo, 0, time.Millisecond); err == nil {
		t.Error("an unreadable namespace check must fail, not degrade to a skip")
	}
}

// The Secret is written by ESO from a path the harbor-robot-provisioner CronJob
// seeds only after Harbor is serving, so a fresh cluster has a window where it is
// legitimately absent. The read must be RETRIED inside the settle budget rather
// than decided once, which is what made this lane fail on the documented window.
func TestRunAssertHarborRoundTripRetriesTheSecretRead(t *testing.T) {
	oN, oS := harborauth.NamespaceExists, harborauth.ReadRobotSecret
	t.Cleanup(func() { harborauth.NamespaceExists, harborauth.ReadRobotSecret = oN, oS })
	harborauth.NamespaceExists = func(string) (bool, error) { return true, nil }
	reads := 0
	harborauth.ReadRobotSecret = func(string, string) ([]byte, error) {
		reads++
		return nil, nil // always absent
	}
	_ = Run("ns", "name", "", ProbeRepo, 50*time.Millisecond, 10*time.Millisecond)
	if reads < 2 {
		t.Errorf("the Secret was read %d time(s) — absence must be retried within the settle budget, "+
			"or the gate decides during the window ESO is documented to need", reads)
	}
}
