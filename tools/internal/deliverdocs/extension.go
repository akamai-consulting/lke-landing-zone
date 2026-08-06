package deliverdocs

// extension.go — `deliver-docs` declares itself.
//
// TWENTY-EIGHTH EXTENSION, AND THE ONE THAT ADDED A WORD. Three earlier
// extractions (`gen-toc`, `guard-docs`, `promote-pipeline`) each wrote the
// operator's repo, each declared `read-repo`, and each recorded the gap and
// refused to invent `write-repo` on the stated grounds that two cases say the
// vocabulary has a hole and do not say what shape it is. All three were resolved
// by a FILE SPLIT: the package renders bytes, package main calls os.WriteFile.
//
// That answer does not reach this one, and the reason is structural rather than a
// matter of size. deliver-docs does not render bytes for someone else to write. It
// PRUNES A DIRECTORY and rewrites links in place, deciding per file, mid-walk,
// from that file's inode identity and whether the template owns its path. Hoisting
// the writes means buffering every rewritten file to hand back, or passing main a
// callback that writes — the write happening inside the package with extra
// indirection and a worse boundary.
//
// The model's own rule is that a gap making a declaration INCOMPLETE is fixed by a
// file split, and one making it IMPOSSIBLE justifies a new word. Three times it
// was the former. This is the latter. See the Grant block in
// tools/internal/extension/extension.go for the argument and what the earlier
// refusals bought.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `deliver-docs` declaration.
//
//	transition:scaffolded/deliver   [read-repo, write-repo]
//	transition:upgraded/redeliver   [read-repo, write-repo]
//
// TWO BINDINGS FOR ONE PIECE OF CODE, which no earlier extension has needed, and
// the first case where a split is driven by the MOMENT rather than by the
// capability. `guard-charts` settled that two checks sharing a grant and a state
// are one binding; the corollary nobody had reached until now is that identical
// work at two DIFFERENT states is two bindings, because a binding is (kind, state)
// and there is nowhere else for the second moment to go.
//
// It genuinely runs at both, and not as a convenience: copier invokes it from
// `_tasks`, which fire on render (`llz new` → scaffolded) and on `copier update`
// (→ upgraded). The instance-root rewriter is written for the second — it gates
// the walk on template ownership precisely because on update it runs against a
// LIVE instance full of files the template does not own, and an early cut of it
// rewrote a link inside `vendor/`.
//
// WHY NOT own-paths, which is the grant this looks most like. own-paths is a FENCE
// — "copier must not render these bytes" — and ADR 0014 pins .template-manifest as
// the sole authority for it. docs/ is classed `managed`: copier renders it
// wholesale on every update, and this verb prunes what copier just wrote. It wants
// the re-render, so the fence would be exactly backwards. own-paths says nothing
// about who writes; write-repo says nothing about copier. This holds the permit
// and not the fence.
//
// WHY `transition` AND NOT `gate`. A gate is defined by cost and reach — fast,
// local, files only, findings out — and this passes the reach test and fails the
// findings one: it does not report, it changes the tree. `--check` does not exist
// here and would not help if it did; a mode that happens not to mutate does not
// make the binding an observation.
//
// The keep-set itself lives in internal/docsguard, not here, and stays there. It
// is the one place where "what docs an instance carries" is decided, and the guard
// that validates every link AS IT RESOLVES inside the pruned tree is meaningless
// if it runs against a different set than this verb ships.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "deliver-docs",
		Short:  "prune a copied-in docs/ to the operator set and repoint the rest at the pinned template",
		Always: true,
		Bindings: []extension.Binding{
			{
				Kind:   extension.Transition,
				Name:   "deliver",
				State:  extension.Scaffolded,
				Grants: []extension.Grant{extension.ReadRepo, extension.WriteRepo},
			},
			{
				Kind:   extension.Transition,
				Name:   "redeliver",
				State:  extension.Upgraded,
				Grants: []extension.Grant{extension.ReadRepo, extension.WriteRepo},
			},
		},
	}
}
