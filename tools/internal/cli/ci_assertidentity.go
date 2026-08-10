package cli

// ci_assertidentity.go — the capability wiring for the `assert-identity`
// extension (internal/assertidentity).

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertidentity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/identityconfig"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
	sharedopenbao "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
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
		// REQUIRED. Install replaces the whole Deps, so an omitted field is nil —
		// not the package default. This one was omitted, and the team-write lane
		// SIGSEGV'd at loginsmoke.go:194 after completing the entire Keycloak half.
		PortForwardOpenbao: sharedopenbao.PortForward,
		SpecTeams: func() []string {
			var out []string
			for _, t := range identityconfig.SpecTeams() {
				out = append(out, t.Name)
			}
			return out
		},
	})
}
