package mutate

// extension.go — `dev-mutation-testing` declares itself, and is the first
// extension that is not about the platform at all.
//
// THIRTY-EIGHTH EXTENSION. Everything extracted so far attaches to something the
// platform does: rendering an instance, provisioning a cluster, converging it,
// asserting it holds. This one measures THE REPOSITORY'S OWN TESTS. Its subject is
// the development process, not the deployment — and that turns out to matter for
// the declaration rather than being a curiosity.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `dev-mutation-testing` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE, WHICH IT EARNS ON REACH AND FAILS ON COST. A gate is defined as fast,
// local, files only, findings out. Three of those four are exactly right: it
// touches no cluster, no cloud and no credential, it reads the repo, and it exits
// non-zero on a NEW survivor. The fourth is not — a mutation run is the slowest
// thing in this catalog by an order of magnitude, minutes where every other gate
// is milliseconds.
//
// It is still a gate. "Fast" in that definition is doing the work of "cheap enough
// to run before you attempt the state", and this is opt-in (`Always: false`), so
// it never sits in an instance's critical path. Recording the mismatch rather than
// widening the definition: the cost clause exists to keep gates out of the
// bootstrap path, and an opt-in developer tool is not in it.
//
// `read-repo` COVERS RUNNING `go test` AND `gremlins` OVER THE REPO, and that is
// worth stating because it looks like it should need more. Both are local
// processes over local source; neither reaches a network, and the grant vocabulary
// has no word for "spawns a subprocess" because spawning one is not a capability —
// what the subprocess TOUCHES is. `posture-plaintext` reaches the same conclusion
// from the other side, holding read-repo while doing nothing but read files.
//
// WHAT IT VALIDATES IS ITSELF, and that is the reason this exists at all. Every
// gremlins failure mode found so far surfaced as a FLATTERING NUMBER rather than
// an error: `package main` never spawns a test process and marks every mutant
// killed; module-copy isolation kills tests that reach above the module root and
// -failfast marks the rest killed; an unset timeout coefficient produces mutants
// that time out and are counted as killed. Three separate bugs, three confident
// "100% efficacy" reports. So the command refuses to report a score until a
// control run is green, a provably-equivalent canary mutant comes back LIVED, and
// nothing timed out — the score being the one signal that cannot detect a broken
// run.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "dev-mutation-testing",
		Short:  "mutation-test a package, and refuse to report a score until the harness proves itself",
		Always: false,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
