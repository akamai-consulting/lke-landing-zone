package plaintext

// extension.go — `posture-plaintext` declares itself.
//
// THIRTIETH EXTENSION, AND THE CLEANEST BOUNDARY OF THE CAMPAIGN: a real closure
// of ZERO. 984 lines and 719 lines of test moved with no injected capability, no
// stranded fixture and no symbol crossing the boundary in either direction. The
// catalog called it "the best stress test of the vehicle" on the strength of its
// size; it turned out to be the cheapest large thing in package main.
//
// THE MEASUREMENT THAT SAID OTHERWISE WAS WRONG, AND THE BUG IS WORTH KNOWING.
// closure.py first reported 5, and all five were words inside operator-facing
// error strings. It stripped BACKTICK literals before double-quoted ones, so a
// backtick inside a "..." string — which this file has many of, e.g. "run
// `kubectl get secret`" — paired with an unrelated backtick elsewhere and ate
// everything between them, exposing prose to the identifier scan. Ordering the two
// strips the other way took the number to 2, both noise. Every closure in this
// catalog measured before that fix is an UPPER BOUND for the same reason.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `posture-plaintext` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE IS THE STRICTEST THING TO CLAIM, and this earns it: fast, local, files
// only, findings out. No cluster, no cloud, no credential — os.ReadFile and a set
// of regexes over the tree. TestPackageStaysFilesOnly fails if that stops being
// true, which is the same pin `posture-credential-coverage` carries.
//
// ONE BINDING, not one per detected shape. It finds six written patterns and one
// unwritten one (a scrape endpoint with no `scheme:` key, which
// prometheus-operator defaults to http), but `guard-charts` settled that a split
// needs divergent CAPABILITY rather than divergent subject matter. These share a
// grant and a moment and answer one question: is every unencrypted in-cluster hop
// an explicit, reviewed decision.
//
// THE OPEN QUESTION IT RAISES IS ABOUT DATA, NOT CAPABILITY, and the catalog
// predicted it: this is "the most instance-tunable" candidate, because
// `plaintextAllowed` is POLICY rather than fact. Six hundred lines of it are an
// adopter's accepted residuals — which hops they tolerate, why, and who owns
// closing them — and today they are compiled into the binary. An instance that
// meshes its Prometheus cannot delete an entry without a source change, and one
// that ships an extra plaintext hop cannot register it at all.
//
// NOTHING IS PROPOSED HERE, deliberately. The declaration model says where an
// extension attaches and what it may touch; it has no vocabulary for "this
// extension carries per-instance configuration", and inventing one from a single
// case is what this campaign has refused four times (`write-repo` took four cases
// and three refusals; the fifth binding kind is being held at one). It is recorded
// because the extension is `ext? ✔` in the catalog — a candidate for running as a
// pure-argv action later — and an argv action cannot carry a compiled-in registry
// at all. Whoever builds that half meets this question first.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "posture-plaintext",
		Short:  "every unencrypted in-cluster hop is registered, reviewed and still real",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
