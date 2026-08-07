package sustain

// extension.go — `template-sustain` declares itself, and declares itself PARTIAL.
//
// SEVENTH EXTENSION, AND THE ONE THAT WAS EXPECTED TO FAIL. The catalog scores
// `template-sustain` at 1,148 lines and names `own-paths` as its grant; the closure
// census predicted it might not be separable at all, because 7 of `upgrade_policy.go`'s
// references are into `ci_template_manifest.go` — the file ADR 0014's
// one-ownership-authority corollary pins as permanently core.
//
// The prediction held. What moved is everything that ASKS about provenance; what
// stayed is everything that ACTS on the manifest.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `template-sustain` declaration.
//
//	assertion:upgraded [read-repo]  drift — how far behind the template is this instance
//	gate:scaffolded    [read-repo]  the upgrade-churn guard
//
// `upgraded` IS A RECURRING STATE and this is the first binding to attach to one
// other than `destroyed`. It cost nothing: `bindableStates` already permitted both
// kinds there, which is the fourth consecutive case of the ceiling being right
// before it was tested.
//
// WHY THERE IS NO `own-paths` HERE, AND WHY THAT IS THE RESULT. The grant belongs
// to the restore/overwrite pass — the thing that tells copier "do not re-render
// these bytes" — and that pass is `upgrade_policy.go`, which cannot leave package
// main. It reads the manifest's class table (`classify`, `lookupTemplateClass`,
// `scaffoldManifestFiles`, `upgradeOverwrite`, `upgradeRestore`), and ADR 0014
// says there is exactly ONE ownership authority. So the extension that would hold
// `own-paths` is welded to the file that DEFINES `own-paths`.
//
// That is not a blocker to be worked around; it is a finding about the model. A
// grant whose meaning lives in core may not be declarable by anything outside
// core, which would make `own-paths` the one grant no extension can ever hold. The
// alternative — teach the manifest to accept contributed paths — is a design
// question ADR 0014's corollary opens and does not answer.
//
// NINTH CATALOG CORRECTION, AND THE SECOND ABOUT MEMBERSHIP. `managed-fresh`
// arrived here from the `apl-upgrade` row, which the catalog lists as
// "ci_managed_lock 230, ci_prepare_apl_upgrade 76". Those are two unrelated
// things. `prepare-apl-upgrade` annotates the apl-operator Deployment before an
// apl-core chart upgrade — genuinely apl-upgrade, and genuinely cluster-write.
// `managed-fresh` recomputes .template-managed.lock digests over the scaffold: it
// never touches a cluster, and its subject is the TEMPLATE, which is this
// extension. The row grouped them because both run near an upgrade.
//
// `deliver-docs` and `chart-publish` came out of `release-publish` the same way,
// for the same reason: a row grouped by WHEN things run rather than by what they
// touch. That is now three files across two rows.
//
// AND THE SECOND CASE FOR `Incomplete`. `reconcile-actions` was the first
// extension to arrive partial; this is the second, with a different cause. Two
// independent occurrences is the bar this model has used for every vocabulary
// change, so `extension.Extension.Incomplete` exists as of this commit and both
// declarations now say what they are missing.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "template-sustain",
		Short:  "keep an instance in step with the template it was rendered from",
		Always: true,
		Bindings: []extension.Binding{
			{
				// `llz drift`: resolve this instance's provenance and compare it
				// against the template's current head. Read-only — it reports, and
				// `--strict` turns the report into an exit code.
				Kind:   extension.Assertion,
				State:  extension.Upgraded,
				Grants: []extension.Grant{extension.ReadRepo},
			},
			{
				// TWO GUARDS, ONE BINDING, per the rule guard-charts settled: a split
				// needs divergent CAPABILITY, not divergent subject matter.
				//
				//   - the churn guard: refuse an upgrade whose diff is larger than the
				//     change it carries.
				//   - managed-fresh: every digest-locked template file still matches
				//     the digest the template shipped, so a hand-edited instance fails
				//     CI instead of silently diverging.
				//
				// Both are files in, findings out, before anything runs — no network,
				// no cluster, no credential. One question in two halves: is this
				// instance still the thing the template rendered.
				Kind:   extension.Gate,
				State:  extension.Scaffolded,
				Grants: []extension.Grant{extension.ReadRepo},
			},
		},
		Incomplete: []string{
			"transition:upgraded[own-paths] — the copier restore/overwrite pass " +
				"(upgrade_policy.go) reads .template-manifest's class table, which ADR 0014 " +
				"pins as the single ownership authority, so the binding that would hold " +
				"own-paths cannot be separated from the file that defines it",
			"template_commit.go and ci_upgrade_test_gate.go — provenance resolution over " +
				"the GitHub API and the copier-update smoke gate, both still in package main",
		},
	}
}
