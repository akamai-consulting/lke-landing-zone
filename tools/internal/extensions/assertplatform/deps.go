package assertplatform

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"

// Deps carries what this package cannot reach for itself.
//
// Small, and it should be: five assertion lanes that observe and report. The only
// capabilities here are "run a diagnostic command" and "read the spec" — nothing
// that writes, which is what an assertion-only extension looks like when the
// declaration is honest.
type Deps struct {
	// ExecCombined runs a command and returns stdout+stderr as one string,
	// ignoring exit status. The health-workflow lane uses it for DIAGNOSTICS: when
	// an Argo Workflow fails, the tool's own error text is the entire value of the
	// failure report, and a stdout-only, error-gated read discards exactly that.
	ExecCombined func(name string, args ...string) string

	// Exec captures a command's stdout. The classified cluster reads go through
	// internal/kubectlprobe, which owns its own seam; this is for the one call that
	// wants raw JSON back rather than a probe verdict.
	Exec func(name string, args ...string) ([]byte, error)

	// LoadSpec reads the instance's LandingZone spec. Injected because the search
	// path is package main's business (it knows where an instance repo keeps its
	// spec), while what the pin MEANS is this package's.
	LoadSpec func() (*clusterspec.LandingZone, bool, error)
}

// deps is the installed capability set, defaulting to implementations that work
// rather than to nil funcs — the action-ABI rule the earlier extractions paid for.
// Installed rather than threaded for the same reason internal/converge does it:
// the lanes are leaf predicates and a capability parameter on each would be noise.
var deps = Deps{
	ExecCombined: func(string, ...string) string { return "" },
	Exec:         func(string, ...string) ([]byte, error) { return nil, nil },
	LoadSpec: func() (*clusterspec.LandingZone, bool, error) {
		return nil, false, nil
	},
}

// Install wires the capabilities main owns. Call once, before any lane runs.
func Install(d Deps) { deps = d }

// firstNonEmpty returns the first non-empty string. A local three-liner rather
// than an import: package main's copy lives in tokens.go, the credential
// provisioning wizard, and there is no behaviour here to drift.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
