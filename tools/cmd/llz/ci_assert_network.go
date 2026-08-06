package main

// ci_assert_network.go — the capability wiring for the `assert-network`
// extension (internal/assertnetwork).

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/assertnetwork"

func init() { installAssertNetworkDeps() }

func installAssertNetworkDeps() {
	assertnetwork.Install(assertnetwork.Deps{
		Exec:         func(n string, a ...string) ([]byte, error) { return execOutput(n, a...) },
		ExecCombined: func(n string, a ...string) string { return execCombined(n, a...) },
	})
}
