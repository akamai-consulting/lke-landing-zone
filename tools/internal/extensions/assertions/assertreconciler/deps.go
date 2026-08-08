package assertreconciler

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

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
	// Cluster is the kubectl handle, SCOPED BY THIS EXTENSION'S DECLARED GRANTS.
	//
	// It replaces the `Exec func(name string, args ...string)` pair that used to
	// live here. Those were a general process-runner: they could invoke `bao`,
	// `linode-cli` or `kubectl delete` regardless of what the binding declared, so
	// this extension's two `cluster-read` grants constrained nothing at all. Both
	// bindings here are read-only, and this handle now enforces that — a `delete`
	// through it returns a refusal naming the grant it would need, before the
	// process starts.
	//
	// Combined() is kept for the node-scheduling diagnostics, where the tool's own
	// error text IS the finding and an error-gated read would discard it.
	Cluster capability.Cluster

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
	// A binding with no grants yields a handle that refuses everything, which is
	// the right default: an un-installed Deps must not silently read a cluster.
	Cluster: capability.For(extension.Binding{}).Cluster,
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
