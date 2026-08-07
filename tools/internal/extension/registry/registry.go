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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/argodiag"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertidentity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertnetwork"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertobjstore"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertplatform"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertreconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertregistry"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertsecrets"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertsuite"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/atrest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoca"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baolifecycle"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoseed"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/bootstrapcluster"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/brownfield"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/budget"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/chartguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/chartpublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusteraccess"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/converge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cosignguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/coverageguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/credcoverage"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/database"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/deliverdocs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/docsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/doctor"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/firewall"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/gameday"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/harbor"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/healthsla"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/identityconfig"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/instanceresolve"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kyverno"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/manifestguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/meshegress"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/monitoringlabel"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/mtlsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/mutate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/objenc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/phasetiming"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/plaintext"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/promote"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/reconcilelanes"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/reconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/releasepublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/seedspecial"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/statepassphrase"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/teardown"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/templatemanifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tofudriver"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tokeninv"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/versionpins"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/volumes"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/wavehealth"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/workflowshells"
)

// declarations is the built-in set. One line per extension; the catalog
// (docs/designs/internal-extensions.md) sizes the other 49.
//
// Listed in import order, NOT declaration order — All sorts by name, so the order
// here carries no meaning and nobody has to maintain one.
var declarations = []func() extension.Extension{
	assertobs.Extension,
	assertsecrets.Extension,
	argodiag.Extension,
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
	baoca.Extension,
	baoseed.Extension,
	baolifecycle.Extension,
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
	instanceresolve.Extension,
	firewall.Extension,
	credcoverage.Extension,
	credrotate.PATExtension,
	credrotate.ObjKeyExtension,
	database.Extension,
	envtopology.Extension,
	docsguard.Extension,
	doctor.Extension,
	healthsla.Extension,
	tofudriver.Extension,
	tokeninv.Extension,
	objenc.Extension,
	chartpublish.Extension,
	gameday.Extension,
	kyverno.Extension,
	manifestguard.Extension,
	mutate.Extension,
	phasetiming.Extension,
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
