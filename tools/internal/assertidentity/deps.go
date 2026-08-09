package assertidentity

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Deps carries what this package cannot reach for itself.
//
// A note on the literals: keycloak.NS/Realm/AdminSecret moved into
// internal/keycloak with the client, but the OpenBao namespace and root pod are
// inlined here rather than injected. They are two strings this lane needs to reach
// a service it does not own, and a Deps field for each would be ceremony — the
// three-clause rule says a constant is not a capability.
//
// Three fields, and the interesting thing is what is NOT here: the Keycloak admin
// client. It could not be injected, because this package defines METHODS on it —
// so it had to become internal/keycloak instead. See extension.go.
type Deps struct {
	// Exec captures a command's stdout.
	Exec func(name string, args ...string) ([]byte, error)

	// SecretField reads one key out of a Kubernetes Secret. The smoke lane needs
	// the platform-admin credential to drive a login.
	SecretField func(ns, name, key string) string

	// SpecTeams lists the teams declared in the LandingZone spec. Owned by the spec
	// loader; this lane needs the names to pick which team's login to smoke.
	SpecTeams func() []string

	// DescribeSecret renders a Secret for a diagnostic when a login fails. Its
	// whole value is the failure path, so a stub returning "" would leave an
	// operator with a bare "login failed".
	DescribeSecret func(ns, name string) string

	// PortForwardOpenbao opens a port-forward to OpenBao and returns the local
	// address plus a stop func. Owned by the openbao verbs — the smoke lane needs a
	// reachable OpenBao to prove a team login can write, and OpenBao has no
	// external ingress.
	PortForwardOpenbao func() (string, func(), error)

	// ManagedDomain discovers the instance's domain suffix. Owned by the openbao
	// configure verb, which derives it from the managed App Platform's own
	// records — this lane only needs the answer to build a Keycloak URL.
	ManagedDomain func() string
}

// caps is the installed capability set; defaults are non-nil and harmless.
var caps = Deps{
	Exec:               func(string, ...string) ([]byte, error) { return nil, nil },
	SecretField:        func(string, string, string) string { return "" },
	ManagedDomain:      func() string { return "" },
	PortForwardOpenbao: func() (string, func(), error) { return "", func() {}, nil },
	SpecTeams:          func() []string { return nil },
	DescribeSecret:     func(string, string) string { return "" },
}

// Install wires the capabilities main owns. Call once, before any lane runs.
func Install(d Deps) { caps = d }

// firstNonEmpty returns the first non-empty string. Pure, localised.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// decodeJSON reads a JSON array/object body, requiring a 2xx status.
func decodeJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// containsString: the definition travelled out of package main with a file this
// extraction moved, leaving both sides using it. Defined here rather than hunted
// for — it is three lines and slices.Contains-shaped.
func containsString(hay []string, want string) bool {
	for _, h := range hay {
		if h == want {
			return true
		}
	}
	return false
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}
