package openbao

// extension.go — `openbao` declares itself: everything this platform does TO its
// secret store, in one place.
//
// THIS IS FOUR EXTENSIONS MERGED, and the split it replaces was the clearest case
// in the catalog of grouping by VERB rather than by capability. `openbao-login`,
// `openbao-lifecycle`, `openbao-seed` and `openbao-peer-ca` were four rows for one
// subject — log in, initialise, put secrets in, exchange peer CAs — and no instance
// would ever enable seeding but not lifecycle. An OpenBao that can be initialised
// but never seeded is not a reduced configuration, it is a broken one.
//
// The split bought nothing at the enablement layer it existed for, and cost a
// reader four packages plus `internal/shared/openbao` and `internal/shared/baoread`
// to answer one question. `guard-charts` already wrote the rule down: a split is
// justified by DIVERGENT CAPABILITY, not by count. Here the capabilities really do
// diverge — which is why this is five NAMED BINDINGS rather than one, and why the
// grant sets below are deliberately not unioned.
//
// TWO THINGS THE MERGE DELETED OUTRIGHT. A second private `firstNonEmpty` (this
// package already had one in quote.go), and `openbao -> baolifecycle`, an
// extension-to-extension import that existed only because `OpenbaoCmd` needed to
// hang `regen-root` off the same cobra parent. A merge that removes an import edge
// and a duplicated helper is the smallest possible evidence that the four were one
// package wearing four names.
//
// WHAT DID NOT MOVE, and the distinction matters when reading an import list:
// internal/shared/openbao is the KV v2 HTTP client and internal/shared/baoread is
// the fail-closed reader. They are substrate that several other extensions call.
// This is the declaration and the verbs. Files needing both alias this one
// `openbaoext`, as they already did.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `openbao` declaration.
//
//	transition:provisioned/init      [cluster-read, cluster-write, secret-custody, cloud-mutate]
//	transition:provisioned/peer-ca   [cluster-read, cluster-write]
//	transition:seeded/seed           [read-repo, cluster-read, cluster-write, secret-custody]
//	transition:seeded/login          [read-repo, cluster-read, cluster-write, secret-custody]
//	transition:seeded/regen-root     [cluster-read, cluster-write, secret-custody, cloud-mutate]
//	                                 (requires operating)
//
// FIVE BINDINGS, AND THE GRANTS ARE WHY. Collapsing these into one would force the
// union — every lane would hold `cloud-mutate`, and `peer-ca` would gain
// `secret-custody` it must not have. That is the over-granting argument
// `reconcile-actions` made when it split into four invariants, and it is the reason
// the merge stops at the package boundary and does not continue into the bindings.
//
// `peer-ca` IS THE CONTROL CASE. What crosses there is a CA certificate — public
// material letting two OpenBao halves authenticate each other. Nothing mints, holds
// or places a secret, so `secret-custody` would be a false claim and `seeded`
// ("credentials and secret material are in place") would be the wrong moment. Peer
// trust is substrate: it belongs with the cluster coming up, before anything has a
// credential to seed. Four of five bindings here hold custody; this one proves the
// grant is being judged per binding rather than pattern-matched onto the package.
//
// `regen-root` CARRIES `Requires: operating`, and it is the binding that motivated
// that field existing. It restores root access to a platform that is already up: it
// establishes `seeded` and it needs `operating` to already hold. State says what a
// binding ESTABLISHES; Requires says what must ALREADY HOLD.
//
// `login` AND `seed` BOTH HOLD `cluster-write`, AND BOTH WERE UNDECLARED UNTIL THE
// CAPABILITY LAYER LOOKED. `llz openbao exec` shells into the OpenBao pod with a
// ROOT token — policy writes, mount changes, anything root permits — which is a
// cluster write even though nothing patches a Kubernetes object, because what an
// exec may do is bounded by the container rather than by kubectl. The seal-key wait
// annotates a parent Application to force a hard refresh when it wedges. Neither
// could have been seen while both went through a general exec seam.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "openbao",
		Short:  "the platform's secret store: initialise it, exchange peer CAs, seed credentials into it, and obtain tokens from it",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:  extension.Transition,
				Name:  "init",
				State: extension.Provisioned,
				Grants: []extension.Grant{
					extension.ClusterRead, extension.ClusterWrite,
					extension.SecretCustody, extension.CloudMutate,
				},
			},
			{
				// No secret grant, deliberately, and a test pins the absence of both.
				Kind:   extension.Transition,
				Name:   "peer-ca",
				State:  extension.Provisioned,
				Grants: []extension.Grant{extension.ClusterRead, extension.ClusterWrite},
			},
			{
				Kind:  extension.Transition,
				Name:  "seed",
				State: extension.Seeded,
				Grants: []extension.Grant{
					extension.ReadRepo, extension.ClusterRead,
					extension.ClusterWrite, extension.SecretCustody,
				},
			},
			{
				Kind:  extension.Transition,
				Name:  "login",
				State: extension.Seeded,
				Grants: []extension.Grant{
					extension.ReadRepo, extension.ClusterRead,
					extension.ClusterWrite, extension.SecretCustody,
				},
			},
			{
				Kind:     extension.Transition,
				Name:     "regen-root",
				State:    extension.Seeded,
				Requires: extension.Operating,
				Grants: []extension.Grant{
					extension.ClusterRead, extension.ClusterWrite,
					extension.SecretCustody, extension.CloudMutate,
				},
			},
		},
		Incomplete: []string{
			"`bao-breakglass` is declared under regen-root's binding because it is the same " +
				"restore with a different key source (the GitHub-stored recovery quorum rather " +
				"than operator-held shares) at the same state with the same grants. It is not " +
				"a separate binding because nothing about it would read differently.",
			"ENABLEMENT IS PER-EXTENSION, NOT PER-BINDING, and this merge is the first place " +
				"that costs something. `openbao-peer-ca` shipped `Always: false` -- peer CAs " +
				"only matter for an HA PAIR, and a standalone deployment never exchanges one -- " +
				"while the other three were always-on. Merging forces one value and the honest " +
				"one is `true`, because four of the five bindings are unconditional. So " +
				"`peer-ca` is now declared always-enabled and is not: its conditionality lives " +
				"in the code that decides whether to run it, which is exactly the kind of fact " +
				"this model exists to pull OUT of the code. Nothing regresses today because " +
				"nothing dispatches on Always yet. Whoever builds enablement needs " +
				"Binding.Always, or an equivalent, before this note can be deleted -- and this " +
				"is case one, so the two-case bar for changing the model is not met.",
		},
	}
}
