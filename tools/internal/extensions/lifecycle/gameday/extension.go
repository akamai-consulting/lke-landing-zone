package gameday

// extension.go — `wedge-gameday` declares itself, and the declaration is forced
// twice over.
//
// THIRTY-FOURTH EXTENSION, AND THE CLEANEST BOUNDARY SINCE posture-plaintext:
// closure 2 (both noise), one inbound edge (the cobra constructor), and no Deps
// struct — it already took internal/cigate.Deps as a parameter before the move.
// That package came out with `converge` and now has thirteen callers. A capability
// seam that is already shared is one the extraction does not have to invent.
//
// It is deliberate FAULT INJECTION: break one platform ExternalSecret by
// repointing its secretStoreRef at a store that does not exist, then assert the
// blast radius is contained — the carved Application that owns it goes non-Healthy,
// and platform-bootstrap plus every sibling stays Healthy. Before the blast-radius
// decomposition the same broken ExternalSecret starved every later-wave resource
// across all bundles (the #163 class). It always restores the ExternalSecret, and
// refuses to run unless the cluster is Healthy to begin with — you cannot prove
// containment on an already-sick cluster.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `wedge-gameday` declaration.
//
//	transition:converged[cluster-read, cluster-write]
//
// SIXTH TIME THE MODEL HAS FORCED `transition:converged` ONTO A CHECK, and the
// first where BOTH halves of the binding are forced rather than just the kind.
//
// The kind is forced the usual way: this mutates (it patches an ExternalSecret and
// reads back Application health), and an assertion may hold read grants only.
//
// The STATE is the new part. A gameday runs against a platform that is up and
// steady — the natural state is `operating`, and the command says so itself by
// refusing to start unless the cluster is already Healthy. But `bindableStates`
// gives Transition no way to reach `operating`, on the sound general reasoning that
// `operating` is a condition that HOLDS rather than a place you move the platform
// to. That reasoning is right about transitions and wrong about this: the drill
// does not move the platform to `operating`, it requires the platform to already
// BE there.
//
// So `converged` is the nearest legal state and the declaration is true — the
// platform is converged when this runs — while quietly losing the precondition
// that matters. The five earlier cases (`converge`, `assert-observability`,
// `assert-secrets`, `assert-identity`, `assert-objstore`) all genuinely belong at
// `converged`; this one is the first pushed there.
//
// THE PRECONDITION IS NO LONGER LOST. `Requires: operating` is declared below, and
// this paragraph used to end "STILL NOTHING INVENTED". What it asked for is what
// was eventually built, almost verbatim: "not a new binding kind but a way to say
// 'this is a CHECK, it must mutate to run, and it requires state X rather than
// establishing it'. A fifth kind bolted on now would have answered the kind
// question and left the state question exactly where it is." That warning is why
// Requires is a field on Binding and not a fifth BindingKind.
//
// THIS CASE IS ALSO WHAT MADE THE OTHER TWO ARGUABLE. `rotate-admin` and
// `bao-breakglass` had each fallen back to "the state whose credentials I restore",
// resolved identically, and concluded the workaround was a stable convention rather
// than a missing word. This one restores nothing, so it fell back to mere proximity
// instead — which is how the convention turned out to be two conventions, and the
// gap turned out to be real.
//
// OPT-IN, unlike almost everything else here. Deliberate fault injection against a
// healthy cluster is not something an instance should get by default — it is a
// thing an operator chooses to do, on a cluster they are willing to break.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "wedge-gameday",
		Short:  "break one ExternalSecret on purpose and prove the blast radius is contained",
		Always: false,
		Bindings: []extension.Binding{{
			Kind:     extension.Transition,
			State:    extension.Converged,
			Requires: extension.Operating,
			Grants:   []extension.Grant{extension.ClusterRead, extension.ClusterWrite},
		}},
	}
}
