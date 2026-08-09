package environments

// extension.go — `environments` declares itself: everything that decides what
// deployments an instance has.
//
// THIS IS THREE EXTENSIONS MERGED, and the merge is the finding. `env-add`,
// `env-topology` and `env-definition` were separate rows for one subject, split by
// VERB rather than by capability: add an environment, change an environment, write
// the file an environment is. Nobody would ever enable one and not the others —
// an instance that can add a deployment but not describe it is not a coherent
// configuration — so the split bought nothing at the enablement layer it existed
// for, and cost a reader three packages to answer one question.
//
// THE RULE IT VIOLATED WAS ALREADY WRITTEN DOWN. `guard-charts` established that a
// split is justified by DIVERGENT CAPABILITY, not by count: its three checks hold
// the same grant at the same moment, so naming them separately would buy nothing
// their own names do not already say. These three held `read-repo` at
// `configured`, `read-repo` at `configured`, and `read-repo`+`write-repo` at
// `scaffolded`. Two of the three were indistinguishable; the third differs by one
// grant and one state, which is what a NAMED BINDING is for.
//
// WHY `environments` AND NOT `env`. The obvious package name shadows one of the
// most common local variable names in this tree — `env` is a deployment name in
// dozens of signatures and a `*cobra.Command` in main.go. Go would have accepted
// it and the collision would have surfaced as an unrelated-looking compile error
// in whichever file next declared one. Renaming the package is cheaper than
// renaming the variable everywhere it is the right name.
//
// TWO SHARED PACKAGES KEEP THESE NAMES AND ARE NOT THIS. internal/shared/envdef
// (the YAML writer) and internal/shared/envtopology (the deployment model) are the
// libraries this extension and several others call. They are substrate; this is
// the declaration. The distinction matters when reading an import list, because
// before the merge the same identifier meant either one depending on the file.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `environments` declaration.
//
//	transition:scaffolded/definition [read-repo, write-repo]
//	transition:configured/add        [read-repo]
//	transition:configured/set        [read-repo]
//	assertion:configured/topology    [read-repo]
//
// WHY `definition` KEEPS ITS OWN BINDING RATHER THAN FOLDING INTO `add`. It is the
// only one that holds `write-repo`, and it is the only one at `scaffolded`. Folding
// it in would widen the union so that reading the topology carried permission to
// write `landingzone.yaml` — the over-granting argument `reconcile-actions` made
// when it split into four, applied in the other direction.
//
// `write-repo` AT `scaffolded` is one of only two states that grant carries, and
// this is squarely one of them: the package creates `landingzone.yaml` when it is
// absent and writes `environments/<env>.yaml`. Files in a working tree, which is
// exactly the distinction `write-repo` was added to draw against the GitHub API
// writes that take `cloud-mutate`.
//
// WHY `configured` FOR THE OTHER THREE. Topology is part of what "configured"
// MEANS for an instance: which deployments exist and how they pair is the
// configuration every later state acts on. `llz env add` runs before anything is
// provisioned, and `llz env set` is how an operator changes their mind about it.
//
// WHY `add` AND `set` ARE TRANSITIONS AND `topology` IS NOT. Both edit
// environments/<env>.yaml and re-render, so a second run after a spec change
// produces different files. Listing what exists changes nothing.
//
// A FIFTH BINDING WAS WRITTEN AND REMOVED, AND IT IS STILL REMOVED. `branchpolicy`
// locks the infra-<env> GitHub Environment to `main` — a PUT against GitHub's
// deployment-branch-policy API, and honestly `transition:configured[read-repo,
// cloud-mutate]`. Validate() refuses `cloud-mutate` at `configured`, and the bar
// for widening a grantStates row is two independent shipping cases plus an
// argument. There is one. So branchpolicy stayed in its own package where the
// CLI's grant story covers it, and this remains case #1.
//
// NO `cloud-read` DESPITE THE VALUES BEING CLOUD FACTS. Region, node type and
// object-storage cluster arrive already resolved: `config-readiness` is the
// extension that asks Linode (its `inputs-resolve` binding), and it is a separate
// declaration precisely so this one can be a pure file writer with no network.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "environments",
		Short:  "what deployments an instance has: adding them, editing them, pairing them for HA, and writing the YAML they are",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Transition,
				Name:   "definition",
				State:  extension.Scaffolded,
				Grants: []extension.Grant{extension.ReadRepo, extension.WriteRepo},
			},
			{
				Kind:   extension.Transition,
				Name:   "add",
				State:  extension.Configured,
				Grants: []extension.Grant{extension.ReadRepo},
			},
			{
				Kind:   extension.Transition,
				Name:   "set",
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
