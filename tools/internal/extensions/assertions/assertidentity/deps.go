package assertidentity

import (
	"fmt"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
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
	// Writer is the six named cluster mutations, scoped by this extension's
	// declared grants. The mutating calls here used to be assembled as an argv on
	// the general exec seam, which could equally have run `delete namespace` or
	// `exec ... -- sh -c`.
	Writer capability.Writer
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
	// Refuses until installed: an un-installed Deps must not mutate a cluster.
	Writer:        capability.For(extension.Binding{}).Writer,
	Exec:          func(string, ...string) ([]byte, error) { return nil, nil },
	SecretField:   func(string, string, string) string { return "" },
	ManagedDomain: func() string { return "" },
	// AN ERROR, NOT AN EMPTY ADDRESS. Returning ("", noop, nil) hands the lane a
	// blank address it then dials — a confusing downstream failure at best, and a
	// silently-skipped assertion at worst.
	PortForwardOpenbao: func() (string, func(), error) {
		return "", func() {}, fmt.Errorf("assertidentity: PortForwardOpenbao not installed")
	},
	SpecTeams:      func() []string { return nil },
	DescribeSecret: func(string, string) string { return "" },
}

// Install wires the capabilities main owns. Call once, before any lane runs.
//
// IT FILLS OMITTED FIELDS rather than replacing wholesale — the same fix, and
// the same reason, as assertreconciler.Install. `caps = d` means any field the
// caller's literal leaves out becomes Go's zero value (nil), not the fail-closed
// default declared above. internal/cli omitted PortForwardOpenbao and the
// team-write lane died on a nil func call, three assert-suite rounds after the
// identical defect was fixed one package over.
func Install(d Deps) {
	if d.PortForwardOpenbao == nil {
		d.PortForwardOpenbao = caps.PortForwardOpenbao
	}
	if d.Exec == nil {
		d.Exec = caps.Exec
	}
	if d.SecretField == nil {
		d.SecretField = caps.SecretField
	}
	if d.DescribeSecret == nil {
		d.DescribeSecret = caps.DescribeSecret
	}
	if d.ManagedDomain == nil {
		d.ManagedDomain = caps.ManagedDomain
	}
	if d.SpecTeams == nil {
		d.SpecTeams = caps.SpecTeams
	}
	caps = d
}

// firstNonEmpty returns the first non-empty string. Pure, localised.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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

// W returns the Writer, or a refusing one if the field was never populated. A
// Deps built as a struct literal has a nil interface there, and a nil interface
// method call is a panic rather than the permission fault the denied handle
// exists to produce.
func (d Deps) W() capability.Writer {
	if d.Writer == nil {
		return capability.Denied()
	}
	return d.Writer
}
