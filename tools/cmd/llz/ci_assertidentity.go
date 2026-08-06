package main

// ci_assertidentity.go — the capability wiring for the `assert-identity`
// extension (internal/assertidentity).

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/assertidentity"

func init() { installAssertIdentityDeps() }

func installAssertIdentityDeps() {
	assertidentity.Install(assertidentity.Deps{
		Exec:           func(n string, a ...string) ([]byte, error) { return execOutput(n, a...) },
		SecretField:    k8sSecretField,
		ManagedDomain:  discoverManagedDomain,
		DescribeSecret: describeSecretForDiag,
		SpecTeams: func() []string {
			var out []string
			for _, t := range specTeams() {
				out = append(out, t.Name)
			}
			return out
		},
	})
}
