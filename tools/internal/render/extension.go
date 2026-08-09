package render

// extension.go — `render` declares itself: turn the LandingZone spec into the
// tfvars and apl-values artifacts an instance commits.
//
// SEVENTY-SIXTH EXTENSION, AND THE HUB THAT WAS BLOCKING THREE OTHERS. `runRender`
// is why `scaffold.go` could not move, why `llz upgrade` could not, and why
// `checks.go` could not (via hooks.go → runLint). Every one of those was recorded
// here as blocked — on a file that measured CLOSURE 6, of which the only real
// edges were:
//
//	first3   ONE LINE, an alias for linode.RegionShort
//	indent   FIVE LINES, already duplicated once in this repo
//
// Nobody had measured the blocker itself. Four "blocked" calls in this campaign
// have now been overturned, and this is the fifth: the thing three files were
// stuck behind was two helpers totalling six lines.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/extension"

// Extension is the `render` declaration.
//
//	transition:configured[read-repo, write-repo]
//
// `write-repo` at `configured`… IS NOT LEGAL, and the model is right to say so —
// that grant carries `{scaffolded, upgraded}` only. See Incomplete: this renders
// AT configuration time and writes files into the working tree, which is a second
// independent case for the same gap `onboard` opened one move ago from the other
// direction.
//
// WHAT IT ACTUALLY GUARANTEES is worth stating: the committed tfvars are DERIVED,
// never authored. `CheckManifestDrift` and the `--diff` path exist so a hand-edit
// is reported rather than silently overwritten on the next render — the spec is
// the source of truth and this is the only thing that makes that claim true.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "render",
		Short:  "render the spec into the tfvars and apl-values artifacts an instance commits",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Configured,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
		Incomplete: []string{
			"write-repo is TRUE and undeclarable at `configured`: this writes tfvars " +
				"and apl-values files into the working tree, but the grant carries " +
				"{scaffolded, upgraded} only. That is the SECOND independent case for " +
				"this gap — `onboard` is the first, from the credential side. Two cases " +
				"is what the campaign's rule asks for before widening a grantStates " +
				"row, so the next person to look at this can argue it rather than guess.",
		},
	}
}
