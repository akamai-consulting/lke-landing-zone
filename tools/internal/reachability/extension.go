package reachability

// extension.go — `reachability` declares itself: three checks that ask whether
// the things llz needs to talk to can actually be reached.
//
// SIXTY-SEVENTH EXTENSION. `verify` (the post-bootstrap kubectl snapshot),
// `status-preflight` (are the tools present and the context pointed somewhere)
// and `sshcheck` (is the GitOps SSH host in known_hosts) measured 6 outbound
// together, 5 after the enabler below, and every one of those was noise, a
// localisable adapter, or a printer that travels by copy.
//
// THE ENABLER WAS A THREE-LINE FUNCTION IN THE WRONG FILE. `kubectlOut` — four
// callers across three packages — lived in `verify.go`, a file about the `llz
// verify` command, which is not what a kubectl-shaped exec helper is about. It is
// `kubectlprobe.Out` now, and `lookable` went with it as `kubectlprobe.Lookable`.
//
// AND THAT MOVE CREATED A SECOND SEAM, briefly. `lookable` reads
// `kubectlprobe.LookPathFn`; package main kept its own swappable `execLookPath`.
// A test stubbing one left the other reading the real PATH — which passes or fails
// depending on what the developer happens to have installed, the exact failure a
// preflight check exists to catch. main's now delegates. One seam.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `reachability` declaration.
//
//	assertion:converged[read-repo, cluster-read]
//
// AN ASSERTION AT `converged`, not a gate, and the reason is the rule this
// campaign had to be corrected on two moves ago: a gate is CHEAP AND OFFLINE. All
// three of these shell out — kubectl against the live context, `ssh-keyscan`
// against a real host — so however read-only they are, they are not gate material.
//
// `converged` because that is what they are evidence about: `llz verify` is what
// an operator runs after a bootstrap to ask "did this actually come up", and the
// preflight and SSH checks are the same question asked earlier and cheaper.
//
// NO MUTATING GRANT, verified rather than assumed: every call is a `get`, a
// `describe`, an `ssh-keyscan` or a `LookPath`. `read-repo` is for the spec the
// preflight reads to know which context it should be pointed at.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "reachability",
		Short:  "can llz reach the cluster, the tools and the GitOps host it needs",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			State:  extension.Converged,
			Grants: []extension.Grant{extension.ReadRepo, extension.ClusterRead},
		}},
	}
}
