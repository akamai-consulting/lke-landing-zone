package main

// ci_assert_network.go — the capability wiring for the `assert-network`
// extension (internal/assertnetwork).

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertnetwork"

func init() { installAssertNetworkDeps() }

func installAssertNetworkDeps() {
	assertnetwork.Install(assertnetwork.Deps{
		Exec:         execOutput,
		ExecCombined: execCombined,
	})
}
