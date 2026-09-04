package assertplatform

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// Deps carries what this package cannot reach for itself.
//
// Small, and it should be: five assertion lanes that observe and report. The only
// capabilities here are "run a diagnostic command" and "read the spec" — nothing
// that writes, which is what an assertion-only extension looks like when the
// declaration is honest.
//
// The k8s-version lane takes its SPEC read from here (LoadSpec) and its Linode
// client from its own binding's grants instead (see k8sversion.go) — a cloud read
// has to go through the declaration that entitles it, and threading a client into
// this struct would hand every lane a handle only one of them may hold.
//
// LoadSpec's default is the dangerous half of that split: it answers
// (nil, false, nil), which the k8s-version lane reads as "no spec here" and exits
// 0 having checked nothing. That is the seam-nobody-installs shape this repo has
// shipped before. It IS installed — cli/ci_assert_platform.go wires
// clusterspec.Detected — and anyone re-scoping this struct needs to keep it that
// way rather than trusting the default to be inert.
type Deps struct {
	// ExecCombined runs a command and returns stdout+stderr as one string,
	// ignoring exit status. The health-workflow lane uses it for DIAGNOSTICS: when
	// an Argo Workflow fails, the tool's own error text is the entire value of the
	// failure report, and a stdout-only, error-gated read discards exactly that.
	ExecCombined func(name string, args ...string) string

	// Writer is the six named cluster mutations, scoped by this extension's
	// declared grants. The two ephemeral `delete workflow` calls used to go
	// through ExecCombined, which could equally have run `delete namespace`.
	Writer capability.Writer

	// Exec captures a command's stdout. The classified cluster reads go through
	// internal/kubectlprobe, which owns its own seam; this is for the calls that want
	// raw JSON back rather than a probe verdict — two of them now (the Argo sweep and
	// the apl-deployed-version lane), which is why this no longer says "the one call".
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
	// Refuses until installed: an un-installed Deps must not mutate a cluster.
	Writer: capability.For(extension.Binding{}).Writer,
	Exec:   func(string, ...string) ([]byte, error) { return nil, nil },
	LoadSpec: func() (*clusterspec.LandingZone, bool, error) {
		return nil, false, nil
	},
}

// Install wires the capabilities main owns. Call once, before any lane runs.
func Install(d Deps) { deps = d }

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
