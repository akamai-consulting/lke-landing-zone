package upstreamupdates

// extension.go — `upstream-updates` declares itself.
//
// The two verbs behind the automated arrival of upstream changes: `pr-touches`,
// which decides whether a pull request's diff may change what Terraform state
// should contain, and `upgrade-pr`, which turns a CI `llz upgrade` into a
// reviewable pull request.
//
// THEY SHARE AN EXTENSION BECAUSE THEY SHARE A FAILURE MODE, not because they
// share code — they share none. Each one's wrong answer is a silent success:
// pr-touches answering `false` on a broken query skips a state-writing import on
// every PR while looking like a clean tree, and upgrade-pr treating "already
// current" as an upgrade opens an empty PR every month. Both exist to make the
// unattended path fail loudly, and keeping them together is what makes that the
// stated purpose of the package rather than an accident of two files.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `upstream-updates` declaration.
//
//	transition:upgraded[read-repo]
//
// WHY `transition` AND NOT `gate`. pr-touches alone would be a gate: it reads a
// diff and answers a question, changing nothing. upgrade-pr pushes a branch and
// opens a pull request, which is a change to the operator's repo in the plainest
// sense — so the binding follows the more consequential verb. A binding that
// described the pair as an observation would be false for half of it.
//
// `read-repo` is the grant for the same reason `promote-pipeline` carries it
// while writing a workflow file: the vocabulary has no `write-repo`, and this is
// not the change that should invent one. Note what upgrade-pr does NOT do —
// it writes no file in the working tree. Its effects are a git push and a `gh pr
// create`, both of which leave the checkout exactly as `llz upgrade` left it, so
// the gap here is narrower than promote-pipeline's os.WriteFile case.
//
// `upgraded` is the state: both verbs exist to serve the arrival of a new
// template release — one clearing the way for its PR to run the right checks,
// the other opening it.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "upstream-updates",
		Short:  "classify a PR's diff, and open the pull request for a CI-run template upgrade",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Upgraded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
