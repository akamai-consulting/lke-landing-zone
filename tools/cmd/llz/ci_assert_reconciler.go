package main

// ci_assert_reconciler.go — the capability wiring for the `assert-reconciler`
// extension (internal/assertreconciler).
//
// Two of these fields are the coupling the catalog predicted when it listed this
// extension as pairing with `reconciler-runtime`: WithPrometheus reaches the
// metrics the reconciler publishes, and LeaseHolderRenew is the reconciler's own
// Lease parser. When `reconciler-runtime` is extracted, those two are the
// interface it has to keep.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertreconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/reconciler"
)

func installAssertReconcilerDeps() {
	assertreconciler.Install(assertreconciler.Deps{
		Exec:                  execOutput,
		ExecCombined:          execCombined,
		FirewallConfigMapName: reconciler.FirewallConfigMapName,
	})
}
