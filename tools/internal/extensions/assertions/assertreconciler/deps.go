package assertreconciler

import (
	"fmt"
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
	// AN ERROR, NOT A NO-OP, and this one had to change. The header above says
	// defaults should "do something harmless and non-nil" — for a query seam that
	// is neither. Returning nil without querying leaves the probe zero-valued, and
	// a zero-valued reconcilerProbe satisfies healthy(): every failWhy is empty and
	// staleLanes() is empty. So an un-installed WithPrometheus made this lane pass
	// having asked Prometheus nothing, which is precisely the vacuous green the
	// fail-closed doctrine exists to forbid.
	WithPrometheus: func(string, func(func(string) ([]byte, error)) error) error {
		return fmt.Errorf("assertreconciler: WithPrometheus not installed")
	},
}

// Install wires the capabilities main owns. Call once, before any lane runs.
//
// IT FILLS OMITTED FIELDS RATHER THAN REPLACING WHOLESALE, and that is the fix
// for a defect that reached e2e. `deps = d` means every field the caller's
// struct literal leaves out becomes Go's ZERO VALUE — nil — not the fail-closed
// default declared above. The comment promising "defaults that work" was true of
// the var and false of everything after the first Install.
//
// internal/cli's literal set Cluster and FirewallConfigMapName and omitted
// WithPrometheus, so `llz ci assert-reconciler` called a nil func and died with a
// SIGSEGV mid-run, taking the scrape-reconciler lane with it (rc=2). Filling
// instead of replacing turns that class into the fail-closed default: a named
// error, at the point of use, instead of a stack trace — or, worse, silence.
func Install(d Deps) {
	if d.WithPrometheus == nil {
		d.WithPrometheus = deps.WithPrometheus
	}
	if d.Cluster == nil {
		d.Cluster = deps.Cluster
	}
	deps = d
}

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
