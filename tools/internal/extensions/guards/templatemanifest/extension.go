package templatemanifest

// extension.go — `template-manifest` declares itself: the table that says who owns
// each file an instance is rendered from.
//
// FIFTY-FIRST EXTENSION. Closure 2, both noise, 446 lines — it has been separable
// the whole time and was simply never measured. That is worth saying plainly at
// the end of an extraction that spent four iterations on sets that were NOT
// separable: the measurement is cheap and the intuition is unreliable in both
// directions.
//
// WHAT IT OWNS. Every file in a rendered instance falls into a class — managed
// (the template owns the bytes; an upgrade overwrites), instance-owned (an upgrade
// never touches it), and the middle cases. Five callers in package main asked this
// table questions and none of them could be tested against a table that lived
// beside a cobra command.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `template-manifest` declaration.
//
//	gate:scaffolded[read-repo]
//
// A GATE, AND THE LAST WORD IN THE MODEL THIS CAMPAIGN NEEDED. Gates run before a
// state is attempted, over FILES ALONE, and Validate() refuses a gate holding
// anything but read-repo — which is exactly this: it reads the rendered tree and
// the manifest and answers "is every file accounted for, and does its class match
// what the template says?" It writes nothing and touches no substrate.
//
// `scaffolded` rather than `upgraded`, though the classes exist FOR upgrades. The
// question this answers is "is this instance's file ownership well-formed", and
// that must be true from the moment the instance exists — an unclassified file
// discovered at upgrade time is a file that has been silently unowned since it was
// rendered.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "template-manifest",
		Short:  "classify every rendered file as template-owned or instance-owned, and verify the manifest covers the tree",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
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
	panic("template-manifest: no gate binding — the scaffold scan builds its " +
		"read-repo reader from it, so its absence is a wiring bug")
}
