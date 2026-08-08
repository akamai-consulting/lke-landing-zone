package assertsecrets

import (
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// Deps carries what this package cannot reach for itself.
//
// Checked against the three-clause rule: can the package already do it with a
// grant it holds? is it a pure function? is it already injectable elsewhere?
// `credLabelForSecret` failed the second — it is a two-line lower-and-hyphenate,
// localised below — and internal/promwire and internal/kubectlprobe already own
// the Prometheus and kubectl seams, so neither appears here.
type Deps struct {
	// Writer is the six named cluster mutations, scoped by this extension's
	// declared grants. The mutating calls here used to be assembled as an argv on
	// the general exec seam, which could equally have run `delete namespace` or
	// `exec ... -- sh -c`.
	Writer capability.Writer
	// Exec captures a command's stdout.
	Exec func(name string, args ...string) ([]byte, error)

	// ExecCombined runs a command returning stdout+stderr as one string, ignoring
	// exit status. The audit lane reads Loki through a port-forward and needs the
	// forwarder's own error text when it fails to bind.
	ExecCombined func(name string, args ...string) string

	// BroadPATSeedEnabled reports whether this deployment seeds the broad PAT.
	// Owned by the seeding verb: whether a credential is seeded at all is a
	// property of the seeder, and this lane only asks so it can SKIP cleanly rather
	// than fail on an instance that never opted in.
	BroadPATSeedEnabled func(lz *clusterspec.LandingZone, region string) bool

	// WaitJobTerminal blocks until a Job succeeds or fails. Shared with the Harbor
	// provisioner kick; the broad-PAT rotation lane drives an e2e Job the same way.
	WaitJobTerminal func(namespace, name string, budget, interval time.Duration) (succeeded, failed bool)
}

// caps is the installed capability set. Defaults are non-nil and harmless.
var caps = Deps{
	// Refuses until installed: an un-installed Deps must not mutate a cluster.
	Writer:              capability.For(extension.Binding{}).Writer,
	Exec:                func(string, ...string) ([]byte, error) { return nil, nil },
	ExecCombined:        func(string, ...string) string { return "" },
	BroadPATSeedEnabled: func(*clusterspec.LandingZone, string) bool { return false },
	WaitJobTerminal:     func(string, string, time.Duration, time.Duration) (bool, bool) { return false, false },
}

// Install wires the capabilities main owns. Call once, before any lane runs.
func Install(d Deps) { caps = d }

// credLabelForSecret maps a GitHub secret name to the `cred` label the rotation
// metrics carry. Two lines, pure, and a copy rather than a seam — package main's
// lives in the reconciler's token lane and there is no behaviour to drift.
func credLabelForSecret(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

// firstNonEmpty returns the first non-empty string. Same reasoning.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
