package onboard

// extension.go — `onboard` declares itself: the guided path that takes an operator
// from a scaffolded instance to one with working credentials.
//
// SEVENTY-FOURTH EXTENSION, 1,238 lines, AND THE ONE I WAS MOST WRONG ABOUT.
// Two iterations ago this file recorded wizard.go + tokens.go as part of "the CLI
// front-end design task" — work that needed its own PR. It did not. NINE enablers
// dissolved it without anyone touching either file, and the last of them was seven
// and ten lines of `gh` helper.
//
// The lesson is already written at the top of the candidates section and it earned
// its place: measure before believing any label in this file.
//
// WHY THE TWO FILES ARE ONE EXTENSION. `llz tokens` mints and pushes the
// credentials; `llz wizard` is the same work with the questions asked out loud.
// They share `Gather`, `PushSecrets`, `RepoStatus` and the whole token-URL set —
// measured apart they are closure 6 and 13, measured together closure 5, and the
// difference is entirely symbols they pass between themselves.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `onboard` declaration.
//
//	transition:configured[read-repo, cloud-read, cloud-mutate]
//
// `secret-custody` IS MISSING AND IT IS TRUE — see Incomplete. The model refused
// it and the refusal is defensible: grantStates allows custody only at
// `provisioned`, `seeded` and `operating`, because custody earlier "would mean a
// credential exists before anything has been built to issue one, which is the
// shape of a hardcoded secret rather than a fetched one".
//
// THAT RATIONALE ASSUMES THE PLATFORM MINTS THE CREDENTIAL. Here a HUMAN does:
// they create a Linode PAT and a GitHub fine-grained token in a browser and paste
// them in. Nothing has to exist yet, because nothing issued them. That is a real
// gap in the model, not a mislabelled binding — and it is CASE ONE. The campaign's
// rule for widening a grantStates row is two independent shipping cases plus an
// argument, and one case is a hunch. Recorded rather than acted on, exactly as
// `argocd-diagnostics` recorded the missing fifth binding kind.
//
// WHAT CASE TWO WOULD LOOK LIKE: any other lane that accepts an
// operator-supplied credential before the substrate exists. There is none in the
// catalog today, which is itself worth knowing — it means this is the ONLY place
// a human hands llz a secret, and that is a fact about the product, not an
// accident of layout.
//
// `cloud-mutate` at `configured` on the precedent branch-policy and chart-publish
// set: GitHub is configured before Linode is provisioned, and creating the
// environment to push a secret INTO is resolving an input by creating it.
//
// `cloud-read` because the doctor half asks whether the repo, the environment and
// the token scopes are actually there before reporting them healthy.
//
// NO `write-repo`, deliberately. It writes `.env` files into the working tree via
// `WriteEnvFile` — but those are gitignored operator scratch, not instance
// content, and claiming the grant would put this in the same sentence as copier
// rendering a template. The distinction is worth keeping sharp.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "onboard",
		Short:  "mint and push the credentials a scaffolded instance needs, guided or direct",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:  extension.Transition,
			State: extension.Configured,
			Grants: []extension.Grant{
				extension.ReadRepo, extension.CloudRead, extension.CloudMutate,
			},
		}},
		Incomplete: []string{
			"secret-custody is TRUE and undeclarable. This package holds a Linode " +
				"PAT and a GitHub fine-grained token in memory long enough to push " +
				"them into the instance's GitHub environment, but grantStates allows " +
				"custody only at provisioned/seeded/operating — a rule written for " +
				"credentials the PLATFORM mints. These are pasted in by a human, so " +
				"nothing needs to exist yet to issue them. Case one of two; see the " +
				"type comment before widening the row.",
		},
	}
}
