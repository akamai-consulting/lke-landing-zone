// Package registry collects the extensions compiled into this binary.
//
// It is the smallest thing that can be called a registry, and deliberately so.
// There is no loader, no YAML manifest, no enable/disable resolution and no remote
// half — docs/designs/internal-extension-model.md records why each is absent, and
// issue #399 sequences them. What exists here is the one function everything else
// will eventually be built around: "which extensions are there, and are their
// declarations legal?"
//
// EACH EXTENSION DECLARES ITSELF IN ITS OWN PACKAGE and this file only names them.
// The alternative — a central table transcribing every extension's bindings and
// grants — is a hand-maintained list beside the code it describes, which is the
// shape this repo has been burned by before: two copies, one edited. The import
// list below is the only thing that has to be remembered, and forgetting it is
// loud (the extension is simply absent from `llz extension list`) rather than
// quiet (a declaration that no longer matches its code).
//
// ORDER IS BY NAME, not by import. Output that depends on import order is output
// that changes when someone reformats a file.
package registry

import (
	"sort"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertidentity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertnetwork"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertobjstore"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertplatform"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertsecrets"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertsuite"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/buildpreflight"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/manifestguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/reachability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/seedspecial"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/volumes"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/assertreconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/assertregistry"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/brownfield"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/database"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/gameday"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/objenc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/releasepublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/budget"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/chartguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/cosignguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/coverageguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/credcoverage"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/docsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/meshegress"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/monitoringlabel"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/mtlsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/pincoherence"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/plaintext"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/templatemanifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/versionpins"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/wavehealth"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/workflowshells"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/atrest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/bootstrapcluster"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/branchpolicy"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/chartpublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/clusteraccess"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/converge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/deliverdocs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/environments"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/firewall"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/harbor"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/healthsla"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/identityconfig"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/kyverno"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/openbao"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/promote"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/reconcilelanes"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/reconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/statepassphrase"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/teardown"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/tofudriver"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/tokeninv"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// declarations is the built-in set. One line per extension; the catalog
// (docs/designs/internal-extensions.md) sizes the other 49.
//
// Listed in import order, NOT declaration order — All sorts by name, so the order
// here carries no meaning and nobody has to maintain one.
var declarations = []func() extension.Extension{
	assertobs.Extension,
	assertsecrets.Extension,
	assertidentity.Extension,
	assertobjstore.Extension,
	deliverdocs.Extension,
	assertnetwork.Extension,
	assertplatform.Extension,
	assertreconciler.Extension,
	assertregistry.Extension,
	atrest.Extension,
	brownfield.Extension,
	budget.Extension,
	chartguard.Extension,
	clusteraccess.Extension,
	configreadiness.Extension,
	converge.Extension,
	openbao.Extension,
	harbor.Extension,
	identityconfig.Extension,
	reconciler.Extension,
	assertsuite.Extension,
	templatemanifest.Extension,
	versionpins.Extension,
	mtlsguard.Extension,
	seedspecial.Extension,
	bootstrapcluster.Extension,
	meshegress.Extension,
	coverageguard.Extension,
	cosignguard.Extension,
	monitoringlabel.Extension,
	workflowshells.Extension,
	firewall.Extension,
	reachability.Extension,
	branchpolicy.Extension,
	buildpreflight.Extension,
	templatecommit.Extension,
	render.Extension,
	pincoherence.Extension,
	environments.Extension,
	credcoverage.Extension,
	credrotate.PATExtension,
	credrotate.ObjKeyExtension,
	database.Extension,
	docsguard.Extension,
	healthsla.Extension,
	tofudriver.Extension,
	tokeninv.Extension,
	objenc.Extension,
	chartpublish.Extension,
	gameday.Extension,
	kyverno.Extension,
	manifestguard.Extension,
	plaintext.Extension,
	promote.Extension,
	releasepublish.Extension,
	reconcilelanes.Extension,
	statepassphrase.Extension,
	sustain.Extension,
	teardown.Extension,
	wavehealth.Extension,
	volumes.Extension,
}

// All returns every compiled-in extension, ordered by name.
//
// It does NOT validate. Validation is a separate call because the two questions
// have different answers at different times: "what is here" must work even when
// something is malformed, or an operator debugging a bad declaration loses the
// listing that would show them which one it is.
func All() []extension.Extension {
	out := make([]extension.Extension, 0, len(declarations))
	for _, d := range declarations {
		out = append(out, d())
	}
	return sortByName(out)
}

// sortByName is a separate function because of a coverage finding worth keeping:
// with ONE extension registered, sort.Slice never calls its comparator, so a test
// over All() cannot tell name-ordering from insertion-ordering — it passes either
// way, and would keep passing right up until the second extension landed in the
// wrong place. Split out, the guarantee is testable against a scrambled slice
// today. Expect the same shape wherever a collection is asserted about before it
// has two members.
func sortByName(exts []extension.Extension) []extension.Extension {
	sort.Slice(exts, func(i, j int) bool { return exts[i].Name < exts[j].Name })
	return exts
}

// Lookup returns the extension with the given name.
func Lookup(name string) (extension.Extension, bool) {
	for _, e := range All() {
		if e.Name == name {
			return e, true
		}
	}
	return extension.Extension{}, false
}

// Validate checks the whole built-in set: every declaration against the model's
// rules, plus the cross-extension rules ValidateSet owns (today, name collisions).
//
// A test calls this, so a declaration that stops validating fails the build rather
// than the operator. That is the property worth keeping as the set grows: the
// registry is where "all of them are legal" can be asserted once, instead of each
// package remembering to assert it about itself.
func Validate() []error { return extension.ValidateSet(All()) }
