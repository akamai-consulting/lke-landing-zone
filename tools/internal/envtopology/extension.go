package envtopology

// extension.go — `env-topology` declares itself.
//
// TWENTY-FIRST EXTENSION. It owns what a deployment IS in this instance: which
// deployments exist, their HA role and pairing, and the `llz env set` /
// `llz network add` edits that change any of it.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `env-topology` declaration.
//
//	transition:configured "env-set"  [read-repo]
//	assertion:configured  "topology" [read-repo]
//
// A THIRD BINDING WAS WRITTEN AND THEN REMOVED, AND THAT IS THIS EXTRACTION'S
// FINDING. `branchpolicy.go` locks the infra-<env> GitHub Environment to `main`,
// which is a PUT against GitHub's deployment-branch-policy API — a mutation of
// infrastructure this repo does not contain. The honest declaration is
// `transition:configured[read-repo, cloud-mutate]`, and Validate() refuses it:
//
//	"cloud-mutate" may only be asked for at provisioned, seeded, converged,
//	operating, destroyed
//
// THE ROW IS ARGUABLY WRONG. Those five states are where a LINODE cloud exists to
// mutate, and the table was written from a catalog that read `configured` as a
// purely local moment. But GitHub is configured before Linode is provisioned, and
// locking a branch policy is exactly the kind of external mutation a reviewer
// would want declared.
//
// IT WAS NOT WIDENED, because the bar this branch set for a grantStates row is TWO
// independent shipping cases and an argument, and there is currently one. The
// second is predicted — `llz tokens` creates OBJ buckets and GitHub secrets at
// configuration time — but predicted is not shipping, and the two rows widened so
// far (cloud-mutate@operating, secret-custody@provisioned) each had code in front
// of them.
//
// So branchpolicy.go went back to package main, where the CLI's own grant story
// covers it, and this is recorded as case #1. That is the same resolution
// `guard-docs`, `promote-pipeline` and `gen-toc` reached for the missing
// `write-repo` grant — four times now the answer has been "move the code to the
// side whose declaration is true", and that consistency is itself evidence the
// model's boundaries are in roughly the right place even where its vocabulary is
// short.
//
// WHY `configured` FOR BOTH. Topology is part of what "configured" MEANS for an
// instance: which deployments exist and how they pair is the configuration every
// later state acts on. `llz env add` runs before anything is provisioned, and
// `llz env set` is how an operator changes their mind about it.
//
// WHY env-set IS A TRANSITION. It edits environments/<env>.yaml and re-renders —
// a second run after a spec change produces different files. The write itself goes
// through internal/yamledit with cmd/llz owning the call, so `read-repo` stays
// true here for the same reason it does in internal/promote.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "env-topology",
		Short:  "what deployments exist and how they pair for HA, and the spec edits that change it",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Transition,
				Name:   "env-set",
				State:  extension.Configured,
				Grants: []extension.Grant{extension.ReadRepo},
			},
			{
				Kind:   extension.Assertion,
				Name:   "topology",
				State:  extension.Configured,
				Grants: []extension.Grant{extension.ReadRepo},
			},
		},
	}
}
