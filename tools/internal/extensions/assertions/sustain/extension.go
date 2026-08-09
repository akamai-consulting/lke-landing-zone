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
			{
				// THE `--write` HALF, AND IT HAD NO BINDING AT ALL. `llz ci
				// managed-fresh --write` regenerates .template-managed.lock — it
				// os.WriteFiles the scaffold's digests — while this extension
				// declared read-repo on two bindings and nothing else. The
				// declaration said it never changed the tree, and it did.
				//
				// It could not be corrected by adding the grant to either existing
				// binding, and the validator says why in its own words: a gate
				// permits only read-repo, and "an assertion permits only read
				// grants … if it must mutate, declare the mutating half as its own
				// transition binding". This is that binding.
				//
				// `scaffolded` because what the lock records is a property of the
				// scaffold as shipped, which is also where the gate that checks it
				// sits.
				Kind:   extension.Transition,
				Name:   "lock-refresh",
				State:  extension.Scaffolded,
				Grants: []extension.Grant{extension.ReadRepo, extension.WriteRepo},
			},
		},
		Incomplete: []string{
			"transition:upgraded[own-paths] — the copier restore/overwrite pass " +
				"(upgrade_policy.go) reads .template-manifest's class table, which ADR 0014 " +
				"pins as the single ownership authority, so the binding that would hold " +
				"own-paths cannot be separated from the file that defines it",
			"provenance resolution over the GitHub API and the copier-update smoke gate " +
				"were listed here as unextracted; BOTH HAVE SINCE MOVED, and neither came " +
				"to this extension — provenance is now the `template-commit` extension " +
				"(assertions/templatecommit/commit.go) and the smoke gate is " +
				"verbs/upgrade/cobra_upgrade_test_gate.go. Recorded rather than deleted " +
				"because it answers the question the first note raises: the sustain surface " +
				"did not grow to cover them, it was split away from them",
		},
	}
}

// readBinding and writeBinding are the two doors this package's file access goes
// through.
//
// Every binding here declares read-repo, and the two read-only ones declare
// nothing else — so which is chosen cannot widen anything, and readBinding takes
// the gate because the managed-fresh CHECK is the gate. The write is different:
// only `lock-refresh` carries write-repo, and borrowing it for a read would hand
// the check the power to rewrite the lock it is verifying, which is the
// make-your-own-verdict-true shape the model exists to prevent.
func readBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Gate {
			return b
		}
	}
	panic("template-sustain: no gate binding — the scaffold is read through one, " +
		"so its absence is a wiring bug")
}

func writeBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Name == "lock-refresh" {
			return b
		}
	}
	panic("template-sustain: no transition:lock-refresh binding — `--write` builds " +
		"its writer from it, so its absence is a wiring bug")
}
