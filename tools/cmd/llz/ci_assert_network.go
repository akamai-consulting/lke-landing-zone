package main

// ci_assert_network.go — the capability wiring for the `assert-network`
// extension (internal/assertnetwork).

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertnetwork"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

func init() { installAssertNetworkDeps() }

func installAssertNetworkDeps() {
	assertnetwork.Install(assertnetwork.Deps{
		// The Writer comes FROM THE DECLARATION: what this lane may mutate is
		// exactly what assertnetwork's binding declared, not whatever an argv can express.
		Writer:       capability.For(assertnetwork.MutatingBinding()).Writer,
		Exec:         execOutput,
		ExecCombined: execCombined,
	})
}
