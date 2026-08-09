package branchpolicy

// extension.go — `branch-policy` declares itself: lock an instance's infra-<env>
// GitHub environment so it can only deploy from `main`.
//
// SIXTY-EIGHTH EXTENSION. 240 lines, closure 4 before this iteration's enabler
// and 3 after — and the enabler was the same shape as the last two:
// `execArgv` was ten lines with EIGHT callers, living in `commands.go`, a
// 1,100-line file about the `llz` subcommands. A process runner is not a
// subcommand. It is `internal/proc.Run` now.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `branch-policy` declaration.
//
//	transition:configured[read-repo, cloud-mutate]
//
// `configured`, and this is one of the two shipping cases that earned
// `cloud-mutate` that state in the first place. GitHub is configured before
// Linode is provisioned, and resolving an input can mean CREATING one — this
// creates the environment bare if it is absent, then applies the ref policy.
//
// `cloud-mutate` FOR A GITHUB API WRITE, on the same precedent as chart-publish
// and the Harbor secret publishes. `read-repo` because the repo is discovered
// from `.copier-answers.yml` when the caller does not name one.
//
// WHAT IT DOES WHEN GITHUB SAYS NO IS THE INTERESTING PART. Environment
// protection rules are a paid feature on private repos, so
// `errEnvProtectionUnsupported` is a NAMED error and `WarnUnsupported` prints a
// warning rather than failing the run. A landing zone that refuses to scaffold on
// a free plan would be worse than one that scaffolds and says the lock is not
// available — and the distinction only survives because the plan-limit response
// is matched deliberately (`isPlanLimitErr`) instead of every 4xx being fatal.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "branch-policy",
		Short:  "lock the infra-<env> GitHub environment to deploy only from main",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Configured,
			Grants: []extension.Grant{extension.ReadRepo, extension.CloudMutate},
		}},
	}
}
