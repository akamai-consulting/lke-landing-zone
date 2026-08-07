package main

// ci_assertsecrets.go — the capability wiring for the `assert-secrets` extension
// (internal/assertsecrets).

import (
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertsecrets"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoseed"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

func init() { installAssertSecretsDeps() }

func installAssertSecretsDeps() {
	assertsecrets.Install(assertsecrets.Deps{
		Exec:         func(n string, a ...string) ([]byte, error) { return execOutput(n, a...) },
		ExecCombined: func(n string, a ...string) string { return execCombined(n, a...) },
		BroadPATSeedEnabled: func(lz *clusterspec.LandingZone, region string) bool {
			return baoseed.BroadPATSeedEnabled(lz, region)
		},
		WaitJobTerminal: func(ns, name string, budget, interval time.Duration) (bool, bool) {
			return assertsecrets.WaitJobTerminal(ns, name, budget, interval)
		},
	})
}
