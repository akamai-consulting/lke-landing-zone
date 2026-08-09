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
// RESOLVED, BY A THIRD CASE THAT BROKE THE ARGUMENT ABOVE. This file used to end
// here with a refusal, and the refusal was well-reasoned: `rotate-admin` had hit
// exactly this and resolved it by declaring the state whose credentials it
// restores rather than the state it runs in, this did the same, and two
// independent cases resolving the SAME WAY is a convention rather than a missing
// word. The campaign's bar is a declaration that is IMPOSSIBLE, and imprecision
// two cases agree about is merely imprecise.
//
// The third case is `wedge-gameday`, and it does not follow the convention. It
// declares `converged` because that is the NEAREST LEGAL state — proximity, not
// semantics — where these two picked the state whose credentials they restore.
// So the workaround was never one convention; it was two, and which one a case
// used depended on whether it happened to restore anything. A convention the third
// instance does not follow is not a convention.
//
// The other half of the evidence is that the two ceiling tables actively CONTRADICT
// each other on `rotate-admin`: grantStates puts `operating` in the secret-custody
// row explicitly for rotation, while bindableStates bars a transition there.
// Imprecision does not explain a model that gives two answers.
//
// So Binding.Requires now carries the precondition, and the state line goes back to
// meaning one thing. Note what did NOT change: the kind is still `transition` and
// `operating` is still unreachable as a State. See the Requires comment on Binding
// for why a fifth KIND would have answered the wrong question.
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
				Kind:     extension.Transition,
				Name:     "bao-regen-root",
				State:    extension.Seeded,
				Requires: extension.Operating,
				Grants: []extension.Grant{
					extension.ClusterRead, extension.ClusterWrite,
					extension.SecretCustody, extension.CloudMutate,
				},
			},
		},
		Incomplete: []string{
			"`bao-breakglass` is declared under bao-regen-root's binding because it is the " +
				"same restore with a different key source (the GitHub-stored recovery quorum " +
				"rather than operator-held shares). The rest of this note used to say the " +
				"model could not express that it runs while the platform is `operating`; " +
				"Binding.Requires now does, and the binding declares it.",
		},
	}
}
