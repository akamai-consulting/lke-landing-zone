package upstreamupdates

// extension.go — `upstream-updates` declares itself.
//
// `upgrade-pr` turns a CI `llz upgrade` into a reviewable pull request.
//
// EVERY DECISION IT MAKES HAS A WRONG ANSWER THAT LOOKS LIKE SUCCESS, which is
// why the judgement is Go with tests rather than shell in the workflow:
// "already current" mistaken for an upgrade opens an empty PR every month; a
// branch mistaken for an open PR reports "already open" forever after one failed
// create; a summary written from the DECISION rather than the outcome claims a
// pull request that does not exist. Each of those was a real defect found in
// review, and none of them fails loudly on its own.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `upstream-updates` declaration.
//
//	transition:upgraded[read-repo]
//
// WHY `transition` AND NOT `gate`. It pushes a branch and opens a pull request,
// which is a change to the operator's repo in the plainest sense. A `gate`
// binding would describe it as an observation, which is false.
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
		Short:  "open the pull request for a template upgrade CI has just committed",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Upgraded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
