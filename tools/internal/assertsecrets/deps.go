package assertsecrets

import (
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// Deps carries what this package cannot reach for itself.
//
// Checked against the three-clause rule: can the package already do it with a
// grant it holds? is it a pure function? is it already injectable elsewhere?
// `credLabelForSecret` failed the second — it is a two-line lower-and-hyphenate,
// localised below — and internal/promwire and internal/kubectlprobe already own
// the Prometheus and kubectl seams, so neither appears here.
type Deps struct {
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

// realGHAAppend is the default Summary contract this package's lanes use when
// they publish a verdict. Real, not a no-op — an installed default is a fixture
// too.
func realGHAAppend(envVar string, lines ...string) error {
	path := os.Getenv(envVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}
