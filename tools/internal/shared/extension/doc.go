// Package extension is the declaration model for LLZ's internal extensions: what
// an extension IS, where it attaches to the platform lifecycle, and what it is
// allowed to touch.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE SPINE, IN ONE PLACE
//
// Everything below this block is derivation. This is the shape.
//
// AN EXTENSION IS A NAME AND A LIST OF BINDINGS. It carries no code. Its Go entry
// points are ordinary functions and cobra commands in its own package; the
// declaration says what those are ALLOWED to do, not how they are called.
//
// A BINDING IS THREE ANSWERS: a KIND (how it attaches), a STATE (where), and
// GRANTS (what it may touch). Grants live here, on the binding, and never on the
// extension — an extension holding a read assertion and a write transition must
// not be able to use the transition's grant from inside the assertion.
//
//	kinds (4)   transition  ACTS to move the platform into its state
//	            assertion   contributes evidence the state HOLDS — read grants only
//	            invariant   must hold continuously; `operating` only
//	            gate        runs BEFORE a state, over files alone — read-repo only
//
//	states (10) scaffolded → configured → provisioned → seeded → converged →
//	            verified → operating, plus promoted / upgraded / destroyed, which
//	            recur rather than sitting on the spine
//
//	grants (9)  read-repo, cloud-read, cluster-read, secret-read  (read)
//	            write-repo, cluster-write, cloud-mutate, secret-custody, own-paths
//
// THREE TABLES ARE THE CEILING, all in validate.go, and every rule about what may
// be declared is an edit to one of them rather than a rule scattered through the
// validator:
//
//	bindableStates    which states each KIND may attach to
//	requirableStates  which states a binding may name as a PRECONDITION
//	grantStates       where each mutating GRANT may be asked for
//
// A row is earned by an extraction that needed it, never by seeming plausible.
// Each table's comment records which case bought which row.
//
// WHAT CONSUMES A DECLARATION TODAY — three things, and it is worth knowing which
// is which, because only one of them is dispatch:
//
//	registry/gates.go   RUNS gate bindings. `llz ci gates` drives the whole set
//	                    from the table there; `make llz-gates` is how CI calls it.
//	registry/           enablement.go resolves an instance's enabled set from
//	                    spec.components; registry.Commands() pins that every verb an
//	                    extension exposes is reachable in internal/cli's tree.
//	shared/capability   builds the HANDLES a binding's grants entitle it to —
//	                    capability.For(binding), CloudFor, RepoForGate. A grant is
//	                    the handle, so a binding that declares nothing is handed
//	                    nothing and can do nothing.
//
// ONE OF THE FOUR KINDS DISPATCHES FROM THE REGISTRY. Gates do. Assertions and
// transitions are hand-wired into the cobra tree in cmd/llz (ci.go, commands.go);
// invariants are scheduled by the in-cluster reconciler. Kind is therefore a real
// constraint for the validator and a real dispatch key for exactly one kind, and
// saying so plainly is cheaper than a reader inferring otherwise from the model's
// symmetry.
//
// THE ACTION ABI IS STILL ABSENT, and gates.go now argues it may stay that way:
// every binding acquires its capability by looking ITSELF up from its own
// declaration at the point of use, which turned out to do something a dispatcher
// cannot — teardown narrows itself to a read-only handle from `--dry-run` at
// RUNTIME, after flags are parsed. Read that file's header before building one.
// ─────────────────────────────────────────────────────────────────────────────
//
// THE MODEL'S DERIVATION — why these states, why these kinds, why grants sit on
// the binding — is in docs/designs/internal-extension-model.md, with the
// measurements it rests on in docs/designs/internal-extensions.md and the budget
// it relieves in docs/adr/0014-core-surface-budget.md. None of that is restated
// here.
//
// WHY IT IS A PACKAGE AND NOT MORE OF package main: the framework that relieves
// core-surface pressure cannot itself add to it. A relief valve plumbed into the
// core relieves nothing.
//
// WHY INTERNAL RATHER THAN A REMOTE-FETCH VEHICLE: most catalogued candidates
// need in-process Go — spec types, credential handles, cluster clients — so the
// primary form of an extension is a Go value compiled in and registered, not a
// manifest fetched from a git ref. A declarative manifest is the projection for
// the externalisable minority and is deliberately not part of this package yet.
//
// SCOPE OF THIS PACKAGE — declaration only. It answers "is this extension
// well-formed and is it allowed to do what it claims?" It does not load, order or
// run anything; the registry next door does the running, and shared/capability
// turns a declaration into a handle. Keeping those apart is what lets this package
// stay dependency-free, which the boundary rule below pins.
//
// AND Validate() IS A BUILD-TIME LINT. Only a test calls it — no `llz` code path
// validates a declaration before using it, because the declarations are
// compiled-in Go values that cannot arrive malformed after the build. The
// consequence is worth being explicit about: the three ceiling tables constrain
// what anyone may WRITE, not what the binary will DO. capability.For reads a
// binding's grants directly and never asks whether the binding is legal. See
// registry.Validate for the longer form.
//
// BOUNDARY RULE. This package must not import cmd/llz (it is a library and must
// not depend upward) and must not import a concrete cloud (internal/linode) — the
// same rule ADR 0013 establishes for the APL layer, enforced here by
// boundary_test.go for the same reason: nothing else would notice. It imports
// nothing but `strings`, `fmt` and `sort`, which
// TestDeclarationModelStaysDependencyFree pins: every extension depends on this
// package, so anything it reaches becomes a dependency of all of them.
package extension
