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
//	gate:scaffolded      "rendered-manifests" [read-repo]
//	assertion:configured "apl-schema"         [read-repo, cloud-read]
//
// TWO BINDINGS, AND THE SPLIT WAS FOUND BY A TEST RATHER THAN BY READING. The
// first cut declared all three lanes as one `gate:scaffolded[read-repo]`, on the
// rule `guard-charts` settled — a split needs divergent CAPABILITY, not divergent
// subject matter. TestPackageStaysFilesOnly then failed on `apl_schema.go reaches
// os/exec`, and it was right: that lane shells out to `helm` to resolve the
// apl-core chart before validating committed values against its schema, so it
// needs a chart the registry has to serve.
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
// THE FOURTH LANE DID NOT COME, and Incomplete says so. `dropped-apiversions`
// is entangled with checks.go in BOTH directions — it calls
// scanDroppedAPIVersions / reportDroppedAPIVersions / scannedManifestTrees, which
// checks.go defines, and checks_test.go in turn drives runCIDroppedAPIVersions.
// Moving it means moving a piece of `llz check`, which is a different extension's
// territory. Same shape as `reconcile-actions` (four invariants declared, four
// lanes still in main) and `template-sustain`.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-manifests",
		Short:  "rendered manifests say what we meant — no duplicate Helm params, no leftover placeholders, apl-values valid against the chart",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Gate,
				Name:   "rendered-manifests",
				State:  extension.Scaffolded,
				Grants: []extension.Grant{extension.ReadRepo},
			},
			{
				Kind:   extension.Assertion,
				Name:   "apl-schema",
				State:  extension.Configured,
				Grants: []extension.Grant{extension.ReadRepo, extension.CloudRead},
			},
		},
		Incomplete: []string{
			"the dropped-apiversions lane has not moved. It is entangled with checks.go in " +
				"both directions — it calls that file's scanDroppedAPIVersions, " +
				"reportDroppedAPIVersions and scannedManifestTrees, and checks_test.go drives " +
				"its entry point — so extracting it means moving a piece of `llz check`, which " +
				"belongs to a different extension.",
		},
	}
}
