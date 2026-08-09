package docsguard

// extension.go — `guard-docs` declares itself, next to the code it describes.
//
// SECOND EXTENSION, AND IT WAS PICKED TO DISAGREE WITH THE FIRST. `guard-budgets`
// fit the model perfectly, which proves less than it looks: a model tested only
// against the case it was derived from is a tautology. This one was chosen because
// the catalog had never scored it (it did not exist when the catalog was taken —
// see the rebase re-measurement) and because it has a half that does not fit. Both
// of those turned into findings; see WHAT IS NOT DECLARED below.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `guard-docs` declaration.
//
// `gate:scaffolded[read-repo]`, the same shape `guard-budgets` takes, and for the
// same reason: file-in, findings-out, over a checkout with nothing resolved. The
// two together are what make the gate group in the catalog credible as a group —
// six more are listed and every one of them should land here unchanged.
//
// WHAT IS NOT DECLARED, AND WHY IT IS A FINDING RATHER THAN AN OVERSIGHT.
// `llz ci gen-toc` in its default mode WRITES Markdown back to disk, and no
// binding here covers that. It is not laziness — the model has nothing to express
// it with:
//
//   - A `gate` may hold `read-repo` and nothing else, so the write cannot hang off
//     this binding. The validator is right to say so; a gate that mutated in the
//     pre-commit path is exactly what that rule exists to stop.
//   - `own-paths` is the nearest-looking grant and is the WRONG one. Per the
//     catalog's Decision 1 it means "copier must not render these bytes, something
//     else does" — a fence, not a write permit. The template repo's own docs/ are
//     not copier-rendered at all, so claiming it here would be a false statement
//     about ownership, and false-but-plausible is the failure mode that grant was
//     corrected to avoid.
//   - There is no `write-repo`. That is the gap, and this is the SECOND case of it:
//     the catalog's `promote-pipeline` generates `promote.yml` and is recorded as
//     holding `read-repo` only, which is already wrong on its face for the same
//     reason.
//
// Two independent cases is enough to say the vocabulary has a hole and not enough
// to know its shape, so nothing is invented here. The honest state is that the
// write mode is author tooling on this repo — it never runs on an instance and
// moves no platform state — and that the next extension which genuinely writes
// instance files should settle whether `write-repo` exists. The file split follows
// the declaration rather than the other way round: rendering is in this package,
// the `os.WriteFile` is in the command, so what IS declared is true.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "guard-docs",
		Short:  "fail when the docs name a command, flag, workflow input or path that does not exist",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Gate,
			State:  extension.Scaffolded,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
