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

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `render` declaration.
//
//	transition:configured[read-repo, write-repo]
//
// `write-repo` at `configured` WAS NOT LEGAL WHEN THIS LANDED, and this extension
// is half of why it is now. The grant carried `{scaffolded, upgraded}` only; this
// renders AT configuration time and writes files into the working tree, which was
// the second independent case for that gap — `environments`' `set` binding is the
// first. Two cases is what the campaign's rule asks for, so the row was widened
// rather than the declaration bent. See validate.go's FOURTH WIDENING.
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
			// `write-repo` BECAUSE PRODUCING FILES IS THE WHOLE JOB. writeTargets
			// os.WriteFiles the rendered values and manifests into the instance
			// repo; the binding claimed read-repo alone. Together with
			// `environments`' set binding this is the second of the two shipping
			// cases that widened grantStates' write-repo row to `configured` —
			// see validate.go, which records why one case was not enough.
			Kind:   extension.Transition,
			State:  extension.Configured,
			Grants: []extension.Grant{extension.ReadRepo, extension.WriteRepo},
		}},
		// NO Incomplete NOTE, and its REMOVAL is the point worth recording. This
		// carried one saying `write-repo` was "TRUE and undeclarable at `configured`"
		// — accurate when written, and left behind when the row it asked for was
		// widened and this binding took the grant. A partial-declaration marker that
		// outlives its gap is worse than none: `llz extension list` renders `◐` off
		// this field, so the listing reported `render` as under-declared while the
		// declaration was complete. That is the misreading Incomplete exists to
		// prevent, running backwards. Closing a gap means deleting the note in the
		// same change.
	}
}

// renderBinding is the transition the rendered tree is written through. Looked up
// rather than reconstructed, following objenc's seedBinding.
func renderBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		for _, g := range b.Grants {
			if g == extension.WriteRepo {
				return b
			}
		}
	}
	panic("render: no write-repo binding — writeTargets builds its writer from one, " +
		"so its absence is a wiring bug")
}
