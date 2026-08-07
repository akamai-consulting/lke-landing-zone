package baolifecycle

// extension.go — `openbao-lifecycle` declares itself. THE THIRD AND LARGEST SLICE
// of the catalog row that has resisted moving for four iterations.
//
// FORTY-SIXTH EXTENSION. `openbao-seed` took the seeding third and `openbao-peer-ca`
// the CA pair; what was left was the row's core — init, regen-root, break-glass and
// the idempotent ensure-ready wrapper — and it could not move until TWO other
// things moved first, in this order:
//
//  1. the exec layer (ci_openbao.go -> internal/baoread), because everything here
//     reaches for it and nothing here could leave while it was in package main;
//  2. the gh-secret writers (-> internal/ghsecret), because they STRADDLED: this
//     set defined ghSetSecretFn while ci_baoseed.go and credrotate_deps.go
//     consumed it.
//
// With both settled the set measures 8 outbound, every one of them localisable
// noise. Before them, every probe of these files came back 16-17. The lesson is
// worth stating plainly because it is the campaign's most repeated one: a set that
// measures badly is usually not entangled with the code it names — it is waiting
// on a layer underneath it that nobody has separated yet.
//
// WHAT THE MOVE COST PACKAGE MAIN, in the good direction: every lane here took a
// `globalOpts` and read exactly ONE field from it. They take a bool now, and
// globalOpts is package main's private business again — four functions had been
// carrying a three-field struct to use a third of it.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `openbao-lifecycle` declaration.
//
//	transition:provisioned[cluster-read, cluster-write, secret-custody, cloud-mutate]
//	transition:seeded     [cluster-read, cluster-write, secret-custody, cloud-mutate]
//
// TWO BINDINGS WITH IDENTICAL GRANTS AT DIFFERENT STATES, which looks redundant
// and is not. The grants are identical because all four lanes do the same four
// things — exec into a pod, read its seal state, handle a root token, write a
// GitHub environment secret. The STATES are what differ, and that is the whole
// content of the declaration:
//
//   - `provisioned` is bao-init and bao-ensure-ready. They run once the cluster
//     exists and BEFORE anything is seeded — init is what makes seeding possible.
//   - `seeded` is bao-regen-root and bao-breakglass. Both RESTORE a root token
//     that was revoked or lost; they do not create the platform's secret material,
//     they put back the credential that lets you reach it.
//
// `cloud-mutate` FOR A GITHUB SECRET WRITE, on the precedent chart-publish set
// (thirty-first extraction): GitHub is a cloud whose state these lanes change.
// `write-repo` would be the wrong word — nothing here writes a file in a repo.
//
// THE PRECONDITION AXIS, SECOND SHIPPING CASE. `bao-breakglass` is a RECOVERY
// action: you run it when the platform is already operating and root has become
// unreachable. `operating` is the honest state and the model forbids it —
// bindableStates bars Transition there, because `operating` is a condition that
// holds rather than a place you move to. That refusal is right, and the gap it
// leaves is real: the model has no way to say "acts DURING one state to restore an
// earlier one".
//
// It is recorded, not invented. `rotate-admin` (internal/credrotate) hit exactly
// this and resolved it by declaring the state whose credentials it restores rather
// than the state it runs in. This does the same, and TWO independent cases
// resolving the same way is evidence the workaround is stable — not yet that a new
// word is needed. The campaign's rule for adding one is a declaration that is
// IMPOSSIBLE; this one is merely imprecise, and imprecision that both cases
// resolve identically is a convention, which is cheaper than a fifth axis.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "openbao-lifecycle",
		Short:  "initialize OpenBao, regenerate root from the recovery quorum, and break-glass",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:  extension.Transition,
				Name:  "bao-init",
				State: extension.Provisioned,
				Grants: []extension.Grant{
					extension.ClusterRead, extension.ClusterWrite,
					extension.SecretCustody, extension.CloudMutate,
				},
			},
			{
				Kind:  extension.Transition,
				Name:  "bao-regen-root",
				State: extension.Seeded,
				Grants: []extension.Grant{
					extension.ClusterRead, extension.ClusterWrite,
					extension.SecretCustody, extension.CloudMutate,
				},
			},
		},
		Incomplete: []string{
			"`bao-breakglass` is declared under bao-regen-root's binding because it " +
				"is the same restore with a different key source (the GitHub-stored " +
				"recovery quorum rather than operator-held shares). It runs while the " +
				"platform is `operating`, which the model cannot express for a " +
				"transition; see the type comment on the precondition axis.",
		},
	}
}
