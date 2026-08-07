package identityconfig

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/keycloak"
)

func srvBase(r *http.Request) string { return "http://" + r.Host }

func withScopeWait(attempts int) func() {
	old := keycloak.ScopeAttempts
	keycloak.ScopeAttempts = attempts
	return func() { keycloak.ScopeAttempts = old }
}

// TestWaitForClientScope_AppearsAfterRetries: the ordering guard polls until
// apl-core converges the `openid` scope, instead of racing ahead and wiring a
// scope-less client.
func TestRunCIKeycloakConfigure_Guards(t *testing.T) {
	if err := RunKeycloakConfigure(false, ""); err == nil {
		t.Error("missing --region must error")
	}
	// No spec in the test cwd → SpecTeams() is empty → clean no-op (not a failure).
	if err := RunKeycloakConfigure(false, "primary"); err != nil {
		t.Errorf("no-teams run must be a clean no-op, got %v", err)
	}
}

// TestKeycloakConnect_RetriesUntilServing: keycloak-configure can run before
// apl-core has Keycloak serving, so Connect retries the port-forward +
// admin-token exchange until the server answers instead of skipping on the first 503.
func TestKeycloakConnect_Timeout(t *testing.T) {
	orig := PortForwardFn
	PortForwardFn = func() (string, func(), error) { return "", func() {}, fmt.Errorf("pod not found") }
	defer func() { PortForwardFn = orig }()
	defer withScopeWait(3)()

	_, _, _, err := Connect(&http.Client{}, "u", "p", func(time.Duration) {})
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("persistent failure must time out with an actionable error, got %v", err)
	}
}

// TestKeycloakConnect_FailsFastOnAuthDenied: a 401 from the token endpoint is a
// permanent credential failure (wrong/disabled admin), so Connect returns
// immediately with an actionable error instead of retrying it as a not-ready
// timeout that would mask the real problem.
func countPrefix(ss []string, prefix string) int {
	n := 0
	for _, s := range ss {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}
