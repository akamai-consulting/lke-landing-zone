package cli

// ci_assert_platform.go — the capability wiring for the `assert-platform`
// extension (internal/assertplatform).
//
// Like internal/converge, the lanes export their own cobra constructors and this
// file is the capability set. Same reasoning: five assertion lanes sharing one
// exit contract, and what package main actually owns is HOW this binary reaches a
// cluster and where an instance repo keeps its spec.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertplatform"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// installAssertPlatformDeps hands the extension the capabilities it declares.
// Every field is a real implementation — an assertion whose diagnostic seam
// returns "" reports a failure with no evidence attached, which is the vacuous
// fixture bug in production form.
func installAssertPlatformDeps() {
	assertplatform.Install(assertplatform.Deps{
		// The Writer comes FROM THE DECLARATION: what this lane may mutate is
		// exactly what assertplatform's binding declared, not whatever an argv can express.
		Writer:       capability.MustWriter(assertplatform.MutatingBinding()),
		ExecCombined: execCombined,
		Exec:         execOutput,
		LoadSpec:     func() (*clusterspec.LandingZone, bool, error) { return clusterspec.Detected() },
	})
}
