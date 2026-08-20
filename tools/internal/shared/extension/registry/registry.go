// Package registry collects the extensions compiled into this binary.
//
// It started as the smallest thing that could be called a registry — All() and
// Validate(), answering "which extensions are there, and are their declarations
// legal?" — and it has since grown the two consumers that make a declaration
// load-bearing:
//
//	gates.go       RUNS gate bindings. `llz ci gates` drives the whole set.
//	enablement.go  RESOLVES an instance's enabled set from spec.components, which
//	               gates.go and the assert battery both skip on.
//
// THIS PARAGRAPH USED TO SAY THERE WAS "no enable/disable resolution", and it went
// on saying it after enablement.go landed beside it in this same package. A package
// doc that contradicts a file next to it is worse than no package doc: the model's
// own doc.go and internal-extension-model.md both list enablement as a live
// consumer, so a reader who trusted this one was told the framework was more inert
// than it is.
//
// STILL ABSENT, and these are the real ones: no loader, no YAML manifest, no remote
// half, and no DRIVER — nothing evaluates a required set and names a lifecycle
// state. docs/designs/internal-extension-model.md records why each is absent, and
// issue #399 sequences them.
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
	"reflect"
	"runtime"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertidentity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertnetwork"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertplatform"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertreconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertregistry"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertsecrets"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertsuite"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/buildpreflight"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/manifestguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/reachability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/seedspecial"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/tokeninv"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/volumes"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/budget"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/callerperms"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/chartguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/cosignguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/coverageguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/credcoverage"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/docsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/meshegress"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/monitoringlabel"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/mtlsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/mutabletags"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/pincoherence"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/plaintext"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/runinjection"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/setupgosite"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/sourceref"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/templatemanifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/versionpins"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/wavehealth"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/workflowshells"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/assertobjstore"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/atrest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/bootstrapcluster"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/branchpolicy"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/brownfield"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/chartpublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/clusteraccess"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/converge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/database"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/deliverdocs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/environments"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/firewall"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/gameday"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/harbor"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/healthsla"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/identityconfig"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/kyverno"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/objenc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/openbao"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/promote"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/reconcilelanes"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/reconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/releasepublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/statepassphrase"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/teardown"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/tofudriver"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/upstreamupdates"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// declarations is the built-in set — 62 extensions from 61 packages (credrotate
// declares two). The catalog (docs/designs/internal-extensions.md) is what sized
// it, and the set has since overtaken the ~57 candidates that catalog counted.
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
	setupgosite.Extension,
	mutabletags.Extension,
	callerperms.Extension,
	runinjection.Extension,
	sourceref.Extension,
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
	upstreamupdates.Extension,
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
//
// AND A TEST IS ITS ONLY CALLER, WHICH IS THE DESIGN RATHER THAN AN OMISSION. The
// three ceiling tables in validate.go are a BUILD-TIME ceiling, not a runtime one:
// nothing consults them while a gate or a transition is running, and nothing
// should. A declaration is written by hand, compiled in, and cannot change between
// builds, so the moment to reject an illegal one is before it ships — re-checking
// at startup would spend work on an answer that could not have changed and would
// give an operator an error about a developer's mistake.
//
// THE TABLES, NOT THE KINDS. That distinction was added when capability.For began
// applying the gate and assertion BLANKET rules as it builds handles, and it is
// what keeps the paragraph above true rather than merely old. A table lookup needs
// a state and answers a question about where a binding sits in the lifecycle;
// re-running one at startup is the wasted work described above. A kind's blanket
// rule needs nothing but the kind, and enforcing it where the handle is built is
// how a declaration that never reached this function still cannot contradict
// itself. See the package doc in shared/extension.
//
// Worth stating because the alternative reading is available and wrong: an unused
// exported Validate() looks like wiring somebody forgot, and the honest version of
// "we only lint this" is cheaper than the next reader deciding to "fix" it.
func Validate() []error { return extension.ValidateSet(All()) }

// Package reports the source package each extension is declared in, as a path
// relative to internal/extensions (e.g. "assertions/volumes").
//
// ────────────────────────────────────────────────────────────────────────────
// IT IS DERIVED FROM THE CONSTRUCTOR, NOT WRITTEN DOWN, which is the only reason
// it is worth having at all. A second list mapping name to directory would be the
// hand-maintained copy this registry exists to avoid — and it would be wrong the
// first time a package moved.
//
// WHY AN OPERATOR NEEDS IT. The extension NAME and the package name differ for
// thirty-five of the sixty-seven — half of them: `assert-storage` lives in assertions/volumes,
// `posture-at-rest` in lifecycle/atrest, `import-brownfield` in
// lifecycle/brownfield. Every error message, gate exemption and ratchet entry in
// this tree names the EXTENSION, so a reader holding a failure has no route to the
// code. This closes that without moving sixty-one packages and rotting every
// document that names one.
//
// NO COUNT OF THOSE DOCUMENTS IS GIVEN, and the omission is the finding rather than
// a gap. The sentence carried one for a long time — "thirty-one documents", borrowed
// from the unrelated name/package count beside it. An audit corrected it to 29,
// measured; the same audit then repointed a batch of stale doc paths at
// internal/extensions and pushed the real figure to 31, so the correction was wrong
// before the commit that made it had finished. The number was decoration: it has now
// been wrong in both directions and never once changed what a reader should do. The
// name/package count above it stays because a test pins it. This is what the
// footnote-not-measurement rule in gates.go is warning about, demonstrated twice
// inside the comment that names it.
//
// The bucket (assertions/ guards/ lifecycle/) is deliberately kept in the string.
// It predicts neither the binding kind nor the name — guards/ holds
// posture-plaintext and template-manifest, assertions/ holds two gates — and
// showing it is what makes that visible rather than arguable.
// ────────────────────────────────────────────────────────────────────────────
func Package(name string) (string, bool) {
	for _, d := range declarations {
		if d().Name != name {
			continue
		}
		full := runtime.FuncForPC(reflect.ValueOf(d).Pointer()).Name()
		// ".../internal/extensions/assertions/volumes.Extension" -> the path after
		// the marker, with the trailing ".Func" removed.
		const marker = "/internal/extensions/"
		i := strings.Index(full, marker)
		if i < 0 {
			return "", false
		}
		pkg := full[i+len(marker):]
		if j := strings.LastIndex(pkg, "."); j >= 0 {
			pkg = pkg[:j]
		}
		return pkg, true
	}
	return "", false
}
