package main

// A COPY of assertplatform's assertArgoAppDeps fixture.
//
// ci_bao_seed_seal_key_mutation_test.go drives the same cigate seam, and the
// helper travelled with ci_assert_argo_app_test.go. Copied rather than exported
// for the reason the converge extraction settled: a fixture shared across an
// extraction boundary makes the extracted package a dependency of the CLI's
// tests, which is the coupling the extraction removes.

import (
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cigate"
)

// assertArgoAppDeps builds seam deps: kubectl answers from the script (keyed
// by joined args prefix), a fake clock advanced by sleep.
func assertArgoAppDeps(t *testing.T, script func(call int, args []string) (string, bool)) (cigate.Deps, *int) {
	t.Helper()
	now := time.Unix(0, 0)
	calls := 0
	return cigate.Deps{
		Kubectl: func(args ...string) (string, bool) {
			calls++
			return script(calls, args)
		},
		Now: func() time.Time { return now },
		Sleep: func(d time.Duration) {
			if d <= 0 {
				d = time.Hour // never freeze: a zero interval must fail an assertion, not hang
			}
			now = now.Add(d)
		},
	}, &calls
}
