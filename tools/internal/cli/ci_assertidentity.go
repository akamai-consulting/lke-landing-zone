package cli

// ci_assertidentity.go — the capability wiring for the `assert-identity`
// extension (internal/assertidentity).

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertidentity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/identityconfig"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
)

func init() { installAssertIdentityDeps() }

func installAssertIdentityDeps() {
	assertidentity.Install(assertidentity.Deps{
		// The Writer comes FROM THE DECLARATION: what this lane may mutate is
		// exactly what assertidentity's binding declared, not whatever an argv can express.
		Writer:         capability.MustWriter(assertidentity.Extension().MustBinding("login-smoke")),
		Exec:           execOutput,
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
