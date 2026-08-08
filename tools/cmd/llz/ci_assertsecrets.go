package main

// ci_assertsecrets.go — the capability wiring for the `assert-secrets` extension
// (internal/assertsecrets).

import (
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/openbao"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertsecrets"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

func init() { installAssertSecretsDeps() }

func installAssertSecretsDeps() {
	assertsecrets.Install(assertsecrets.Deps{
		// The Writer comes FROM THE DECLARATION: what this lane may mutate is
		// exactly what assertsecrets's binding declared, not whatever an argv can express.
		Writer:       capability.For(assertsecrets.Extension().Bindings[0]).Writer,
		Exec:         execOutput,
		ExecCombined: execCombined,
		BroadPATSeedEnabled: func(lz *clusterspec.LandingZone, region string) bool {
			return openbao.BroadPATSeedEnabled(lz, region)
		},
		WaitJobTerminal: func(ns, name string, budget, interval time.Duration) (bool, bool) {
			return assertsecrets.WaitJobTerminal(ns, name, budget, interval)
		},
	})
}
