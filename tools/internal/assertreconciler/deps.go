package assertreconciler

// Deps carries what this package cannot reach for itself.
//
// TWO OF THESE FIELDS ARE THE COUPLING THE CATALOG PREDICTED. It listed this
// extension as pairing with `reconciler-runtime` — "a capability and its
// assertion" — and the pairing shows up here as concrete seams rather than as a
// note: WithPrometheus reaches the metrics the reconciler publishes, and
// LeaseHolderRenew parses the Lease it holds. Neither is this package's to own;
// both are what it must be handed to judge the thing it asserts about.
//
// That is a useful shape to have on the record. An assertion extension's Deps is
// a fairly precise inventory of what its subject exposes — and when
// `reconciler-runtime` is extracted, these two seams are the interface it has to
// keep.
type Deps struct {
	// Exec captures a command's stdout. The classified cluster reads go through
	// internal/kubectlprobe; this is for the calls wanting raw output.
	Exec func(name string, args ...string) ([]byte, error)

	// ExecCombined runs a command returning stdout+stderr as one string, ignoring
	// exit status. Used for the node-scheduling diagnostics, where the tool's own
	// error text IS the finding and an error-gated read would discard it.
	ExecCombined func(name string, args ...string) string

	// WithPrometheus opens a query session against the in-cluster Prometheus and
	// hands the callback a getter. Owned by the observability verbs — this package
	// only asks questions of it.
	WithPrometheus func(promSpec string, fn func(get func(apiPath string) ([]byte, error)) error) error

	// FirewallConfigMapName is the firewall controller's ConfigMap name, owned by
	// the firewall verbs and passed in so this package cannot drift from it.
	FirewallConfigMapName string
}

// deps is the installed capability set. Defaults do something harmless and
// non-nil — the action-ABI rule the earlier extractions paid for: hand zero
// values that work.
var deps = Deps{
	Exec:         func(string, ...string) ([]byte, error) { return nil, nil },
	ExecCombined: func(string, ...string) string { return "" },
	WithPrometheus: func(string, func(func(string) ([]byte, error)) error) error {
		return nil
	},
}

// Install wires the capabilities main owns. Call once, before any lane runs.
func Install(d Deps) { deps = d }

// firstNonEmpty returns the first non-empty string. A local three-liner rather
// than an import — package main's copy lives in the provisioning wizard.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
