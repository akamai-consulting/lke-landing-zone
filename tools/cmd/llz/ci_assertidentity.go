package main

// ci_assertidentity.go — the capability wiring for the `assert-identity`
// extension (internal/assertidentity).

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertidentity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/identityconfig"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
)

func init() { installAssertIdentityDeps() }

func installAssertIdentityDeps() {
	assertidentity.Install(assertidentity.Deps{
		Exec:           func(n string, a ...string) ([]byte, error) { return execOutput(n, a...) },
		SecretField:    kube.SecretFieldOf,
		ManagedDomain:  identityconfig.DiscoverManagedDomain,
		DescribeSecret: kube.DescribeSecret,
		SpecTeams: func() []string {
			var out []string
			for _, t := range identityconfig.SpecTeams() {
				out = append(out, t.Name)
			}
			return out
		},
	})
}
