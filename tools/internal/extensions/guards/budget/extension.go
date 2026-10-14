package budget

// extension.go — `guard-budgets` declares itself, next to the code it describes.
//
// WHY THE DECLARATION LIVES HERE and not in a central manifest: the catalog's
// whole thesis is that an extension is a capability plus the statement of where it
// attaches, and separating the two creates a second thing to keep in step. A
// registry that listed every extension's bindings by hand would be exactly the
// hand-maintained list this repo refuses elsewhere — it would rot the first time
// someone added a binding and edited only one of the two files. So the package
// exports its own declaration and the registry only collects them.
//
// WHY THIS ONE IS FIRST. docs/designs/internal-extensions.md ranks it first of
// five, for a reason that is not size: it holds one grant, needs no cluster and no
// credential, and is already unit-tested — so nothing about the extraction is
// entangled with the question of whether the extension model is right. It also
// makes the gate export itself, which is the cheapest available evidence that the
// remedy `llz ci core-surface` prints is followable: the gate said "extract to
// tools/internal/<pkg>", this package is the result, and the budget went DOWN in
// the same commit.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `guard-budgets` declaration.
//
// ONE BINDING, ONE GRANT, and both are the narrowest the model offers. A budget
// gate is file-in / findings-out: it walks the repo, tallies lines against a
// committed number, and prints. It reaches no cluster, holds no credential, and
// mutates nothing — so `gate` is the kind and `read-repo` is the whole of what it
// may touch, which is also the only grant Validate lets a gate hold.
//
// IT ATTACHES AT `scaffolded`, NOT `configured`. Both are legal for a gate, and
// the distinction is real rather than stylistic: these budgets are computable from
// a checkout alone, with no environments resolved, no tokens gathered and no spec
// loaded. `make lint` and the pre-commit hook both run them in exactly that state.
// Declaring `configured` would say the gate needs inputs it does not, and would
// eventually be believed by whatever schedules it.
//
// ALWAYS-ENABLED, and this is the case that shows why universality could never
// have been the thing that distinguishes an extension from core. A ratchet nobody
// can opt out of is the entire mechanism — an instance that disabled this one
// would silently stop measuring the thing ADR 0014 exists to measure. It is
// nonetheless an extension, because what makes it one is that it attaches at a
// declared point with a declared reach, not that it is optional.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-budgets",
		Short:  "cap untestable logic (untestable-loc) and package main's size (core-surface)",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
