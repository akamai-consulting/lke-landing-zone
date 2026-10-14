package manifestguard

// extension.go — `guard-manifests` declares itself, three lanes of four.
//
// THIRTY-SECOND EXTENSION, AND IT WAS NOT THE ONE PLANNED. The handoff note said
// the next move was `env-topology`'s third binding: `chart-publish` had just
// widened `cloud-mutate` to `configured`, so `branchpolicy.go` — moved back to
// package main by that extraction precisely because the row did not exist — could
// finally come out.
//
// MEASURING IT FIRST SHOWED THE PREMISE WAS WRONG. `lockInfraEnvBranchPolicy` has
// three inbound callers and NONE of them is env-topology: it is called from
// tokens.go and wizard.go, the credential-seeding path. env-topology's own comment
// claims the file as its would-be third binding, and the claim is a
// mis-attribution — the extension that WANTED to declare the GitHub write is not
// the extension that calls it. The debt is real and still open; it belongs to
// `forge-env-seed`, whose GitHub API primitives have six other callers and need a
// shared package first (the internal/keycloak shape). Recorded, not attempted.
//
// The lesson is the one the method already states and I nearly skipped: RE-MEASURE
// BEFORE TRUSTING THE PLAN, including a plan I wrote myself one extraction ago on
// the strength of a comment rather than a scan.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `guard-manifests` declaration.
//
//	gate:scaffolded "rendered-manifests" [read-repo]
//
// ONE BINDING NOW, and the history is worth keeping because it argues for the
// rule rather than against it. There were two: this gate, and an
// `assertion:configured "apl-schema" [read-repo, cloud-read]` that shelled out to
// `helm` to resolve the apl-core chart and validate committed values against its
// schema. The split was found by a test rather than by reading —
// TestPackageStaysFilesOnly failed on `apl_schema.go reaches os/exec`, and it was
// right, because that lane needed a chart the registry had to serve.
//
// The assertion is gone because its INPUT is: a rendered apl-core values.yaml,
// which LLZ stopped emitting on the managed App Platform. The package is
// files-only again, which is what makes the gate kind legal for it.
//
// A gate is DEFINED by cost and reach — fast, local, files only, findings out —
// and `posture-credential-coverage` and `posture-plaintext` both earn it. This
// lane does not. The capability really is divergent, so the rule says split, and
// the split is the honest one rather than a concession:
//
//   - gate:scaffolded — argocd-rendered-apps (no rendered Application carries
//     duplicate Helm parameters, where the last writer silently wins) and
//     placeholder-guard (no rendered manifest still carries the template's example
//     hostname). Both are os.ReadFile and regexes.
//   - assertion:configured — apl-schema. The committed apl-values are an INPUT
//     that must resolve, which is what `configured` means, and resolving them
//     needs the chart. `cloud-read` is the same grant `chart-publish` holds for
//     its registry HEAD.
//
// EIGHTH CATALOG CORRECTION. The row groups four lanes under one guard heading;
// one of them is not a guard. That is the second time a row's KIND was wrong —
// `posture-credential-coverage` was filed as an invariant and is a gate — and the
// first time a single row needed two different kinds.
//
// THE FOURTH LANE CAME, and the Incomplete note it used to carry is gone.
// `dropped-apiversions` was recorded here as entangled with checks.go in BOTH
// directions — it called that file's scanDroppedAPIVersions /
// reportDroppedAPIVersions / scannedManifestTrees, and checks_test.go drove its
// entry point. The entanglement turned out to be one of LOCATION, not of logic:
// the whole cluster only ever had callers in checks.go and in the `llz ci` face
// of this same guard. Nothing of `llz check` came with it, and what stayed behind
// is a five-line lint step that now delegates here.
//
// Worth recording because the note was written in good faith and was WRONG in a
// specific way: "it calls symbols checks.go defines" described where the code
// SAT, not what owned it. `reconcile-actions` and `template-sustain` carry
// similar notes; both deserve the same re-measurement before being believed.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-manifests",
		Short:  "rendered manifests say what we meant — no duplicate Helm params, no leftover placeholders, no dropped apiVersions",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Gate,
				Name:   "rendered-manifests",
				State:  extension.Scaffolded,
				Grants: []extension.Grant{extension.ReadRepo},
			},
		},
	}
}

// renderedManifestsBinding is the gate binding the two rendered-tree guards read
// through. Looked up rather than reconstructed, following the same rule as
// objenc's seedBinding: the handles belong to a BINDING, and an extension with
// several must not hand back the union.
func renderedManifestsBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Gate {
			return b
		}
	}
	panic("guard-manifests: no gate binding — the rendered-manifest guards build " +
		"their read-repo reader from it, so its absence is a wiring bug")
}
