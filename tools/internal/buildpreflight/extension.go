package buildpreflight

// extension.go — `build-preflight` declares itself: the check that runs before
// `llz build` dispatches, so a build cannot be started against state GitHub has
// never seen.
//
// SEVENTIETH EXTENSION. Closure 4, and BOTH real edges were enablers rather than
// entanglement — which is now the campaign's most reliable finding:
//
//	resolveInstanceRepo  -> internal/answers.ResolveInstanceRepo
//	existingPaths        -> copied, nine lines
//
// `resolveInstanceRepo` in turn needed `templateName` and `defaultTemplateOrg`,
// two CONSTS with twenty-three references, which are now internal/templateid.
// Facts, the same call baoread.Namespace got.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `build-preflight` declaration.
//
//	gate:configured[read-repo, cloud-read]
//
// AN ASSERTION, NOT A GATE, and the rule is the one the model corrected me on:
// a gate is CHEAP AND OFFLINE and may hold `read-repo` and nothing else. This
// reaches GitHub — ghDefaultBranch, remoteDeploymentPresent, the paged API — so
// `cloud-read` is true and a gate is therefore impossible. Same resolution as
// instance-resolve: a check that needs the network is not pre-commit material.
//
// `configured` because that is the state it guards the exit from — the repo,
// its branches and its environments are what "configured" means here.
//
// WHAT IT ACTUALLY PREVENTS is worth stating: `llz build` dispatches a workflow
// against a REMOTE ref. If the deployment exists only locally, the workflow runs
// against a repo that has never heard of it and fails somewhere unrecognisable.
// This turns that into one sentence at the point of dispatch.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "build-preflight",
		Short:  "refuse to dispatch a build against a deployment GitHub has never seen",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			State:  extension.Configured,
			Grants: []extension.Grant{extension.ReadRepo, extension.CloudRead},
		}},
	}
}
