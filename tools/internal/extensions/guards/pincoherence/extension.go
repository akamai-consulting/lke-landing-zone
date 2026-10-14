package pincoherence

// extension.go — `pin-coherence` declares itself.
//
// EIGHTIETH EXTENSION, AND A TEXTBOOK GATE: it reads one file, decides, and
// returns. No cluster, no network, no clock. That is what lets it hold `read-repo`
// and nothing else and run in the pre-commit path, which is the whole point —
// the skew it catches is created by a `copier update` that did not apply, and the
// cheapest moment to say so is before the commit that carries it.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `pin-coherence` declaration.
//
//	gate:scaffolded[read-repo]
//
// IT WAS WRITTEN AS `gate:upgraded` AND THE MODEL REFUSED IT — correctly. A gate
// runs BEFORE a state is attempted, and this one runs before a COMMIT, over a
// tree that is already scaffolded. `upgraded` is where the defect is CREATED, not
// where the check sits: a fresh scaffold writes both pins at once and they cannot
// disagree, so only an upgrade can move one without the other.
//
// That distinction has no word in the model, which is what Incomplete records.
// The refusal is not a limitation to route around — binding this to `upgraded`
// would have claimed it runs before an upgrade, which would be a lie about when
// it protects you.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "pin-coherence",
		Short:  "fail when .copier-answers.yml's _commit and llz_version name different releases",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
		Incomplete: []string{
			"the binding names where this RUNS, not what it is about. The skew it " +
				"catches can only be created by an upgrade — a fresh scaffold writes " +
				"`_commit` and `llz_version` in one pass — but a gate cannot attach to " +
				"`upgraded`, because a gate precedes the state it names and this follows " +
				"one. The model has no way to say 'gate that runs after X to catch what " +
				"X broke'. This is case one; see internal/verbs/argodiag for the same shape " +
				"from the other direction, where a DIAGNOSTIC runs after a state failed.",
		},
	}
}

// gateBinding is the binding this guard reads through, looked up rather than
// reconstructed so a second binding cannot silently widen what it may read.
func gateBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		if b.Kind == extension.Gate {
			return b
		}
	}
	panic("pin-coherence: no gate binding — reading the answers file builds its " +
		"read-repo reader from it, so its absence is a wiring bug")
}
