// Package extension is the declaration model for LLZ's internal extensions: what
// an extension IS, where it attaches to the platform lifecycle, and what it is
// allowed to touch.
//
// THE MODEL — states, bindings, grants, and the rules between them — is specified
// in docs/designs/internal-extension-model.md, with the measurements it rests on
// in docs/designs/internal-extensions.md and the budget it relieves in
// docs/adr/0014-core-surface-budget.md. None of that is restated here; what
// follows is only what a reader of THIS package needs.
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
// SCOPE OF THIS PACKAGE — declaration only, and it is wired to nothing.
// It answers "is this extension well-formed and is it allowed to do what it
// claims?" It does not load, register, order, or run anything. The action ABI
// (how an extension's Go entry point receives a cluster client, a credential
// handle, a render context) is deliberately absent: there is no consumer yet, and
// inventing the signature before the first real extension needs it is how the
// wrong ABI gets frozen. `converge` and `import-brownfield` are the cases that
// should drive it — see the catalog's suggested first five.
//
// BOUNDARY RULE. This package must not import cmd/llz (it is a library and must
// not depend upward) and must not import a concrete cloud (internal/linode) — the
// same rule ADR 0013 establishes for the APL layer, enforced here by
// boundary_test.go for the same reason: nothing else would notice.
package extension
