package newinstance

// extension.go — `new` declares itself: render a fresh instance from the
// template, and optionally create and push its GitHub repo.
//
// SEVENTY-NINTH EXTENSION, AND THE ONE THAT EMPTIES commands.go. What is left in
// package main is what was always underneath the pile: ~145 lines of dispatchers
// that turn a cobra flag set into a call somewhere else.
//
// THIS IS THE FIRST THING AN ADOPTER RUNS, which is why so much of it is error
// text rather than logic. Three of its functions build nothing but remediation —
// missingRepoOwnerErr, foreignUserOwnerErr, createRepoErr — because `gh repo
// create` fails in three ways that look identical from the outside and have
// completely different fixes, and the operator hitting one has no instance yet to
// diagnose it with.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `new` declaration.
//
//	transition:scaffolded[read-repo, write-repo]
//
// `scaffolded` is defined as "the instance repo exists and is rendered", and this
// is the command that makes that true — the mirror of `upgrade` one move ago,
// which owned the other state `write-repo` carries. Between them the grant's two
// states now each have the command that earns them.
//
// WHAT IS DELIBERATELY ABSENT IS `cloud-mutate`, and the `--push` path is where a
// reader will doubt it. `gh repo create` does create something remote — but the
// substrate model here is Linode, and GitHub is where the instance's SOURCE
// lives, not its infrastructure. Creating the repo that holds the scaffold is the
// same act as writing the scaffold; declaring it a cloud mutation would put this
// binding at `provisioned`, before the repo it renders into exists.
//
// AND THE REFUSAL IS REAL, NOT INCIDENTAL: every remote-touching call routes
// through Gated, which prints the command and returns nil without `--yes`.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "new",
		Short:  "scaffold a new landing-zone instance, and optionally create and push its repo",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo, extension.WriteRepo},
		}},
	}
}
