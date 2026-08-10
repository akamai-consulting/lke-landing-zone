package cli

// ci_assert_reconciler.go — the capability wiring for the `assert-reconciler`
// extension (internal/assertreconciler).
//
// Two of these fields are the coupling the catalog predicted when it listed this
// extension as pairing with `reconciler-runtime`: WithPrometheus reaches the
// metrics the reconciler publishes, and LeaseHolderRenew is the reconciler's own
// Lease parser. When `reconciler-runtime` is extracted, those two are the
// interface it has to keep.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertreconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/platform"
)

func installAssertReconcilerDeps() {
	// THE HANDLE IS BUILT FROM THE DECLARATION, not handed a general exec. Both of
	// assert-reconciler's bindings are `assertion:operating[cluster-read]`, so the
	// handle it receives refuses every write verb — which is what its grant line
	// has always claimed and, until this, did not do.
	//
	// Bindings()[0] rather than the extension: grants are per-binding so that an
	// extension holding a read assertion and a write transition cannot use the
	// transition's grant from inside the assertion. Both bindings here are
	// identical; a package whose bindings DIFFER must install per lane.
	// WithPrometheus IS REQUIRED HERE, and omitting it is not a small mistake.
	// Install REPLACES the whole Deps struct, so every field this literal leaves
	// out reverts to Go's zero value — not to the package's carefully non-nil
	// default. This literal set only two fields, so WithPrometheus was nil and
	// `llz ci assert-reconciler` panicked with a SIGSEGV mid-e2e (rc=2), taking
	// the scrape-reconciler lane with it.
	//
	// The loud crash was the good outcome. Falling back to the package default
	// would have been worse: it returns nil without querying anything, leaving the
	// probe zero-valued — and a zero-valued probe reports healthy() == true. The
	// lane would have gone GREEN having asked Prometheus nothing at all.
	// deps.go's default is now an error for that reason; this is the real one.
	assertreconciler.Install(assertreconciler.Deps{
		Cluster:               capability.MustCluster(assertreconciler.Extension().MustBinding("functional-health")),
		FirewallConfigMapName: platform.FirewallConfigMapName,
		WithPrometheus:        assertobs.WithPrometheus,
	})
}
