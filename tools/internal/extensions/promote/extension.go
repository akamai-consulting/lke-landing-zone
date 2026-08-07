package promote

// extension.go — `promote-pipeline` declares itself.
//
// EIGHTEENTH EXTENSION, AND IT BINDS `promoted` — THE LAST UNCLAIMED STATE. All
// ten lifecycle states now carry at least one binding, which was the open item
// left after `token-inventory` took `configured`.
//
// It generates .github/workflows/promote.yml from each deployment's
// promotion_rank: a static `needs:`-chained workflow, so the runtime is entirely
// GitHub-native. `needs:` is the on-green gate, the infra-<stage> Environment
// protection rules are the approval gate, and "Re-run failed jobs" is the resume.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `promote-pipeline` declaration.
//
//	transition:promoted[read-repo]
//
// THE GRANT LINE IS TRUE BECAUSE THE FILE SPLIT WAS MADE TO KEEP IT TRUE, and
// that is this extraction's finding.
//
// This extension WRITES .github/workflows/promote.yml. `read-repo` alone does not
// say so. The obvious fixes both fail:
//
//   - `own-paths` is the nearest-looking grant and is the wrong one. Per the
//     catalog's Decision 1 it means "copier must not render these bytes" — a FENCE
//     against `copier update`, not a write permit. promote.yml is a copier-rendered
//     `merge` stub, so the fence would also be factually wrong. Validate() rejects
//     it regardless: own-paths is only meaningful at `scaffolded` or `upgraded`.
//   - Inventing `write-repo` was the other option, and was deliberately NOT taken.
//
// WHY NOT INVENT IT. The catalog already reached this exact gap for `llz ci
// gen-toc` and recorded the reasoning: "two independent cases is enough to say the
// vocabulary has a hole and not enough to know its shape, so nothing was invented
// — the file split follows the declaration instead". `guard-docs` resolved it that
// way. This is the THIRD case and it resolved the same way, which is evidence the
// split is a real answer rather than a workaround being repeated.
//
// The count is now much larger than three, and that is worth recording for
// whoever does decide the grant's shape: SIXTEEN non-test files in package main
// call os.WriteFile. If `write-repo` is ever added, those are its candidates, and
// the question it has to answer is which of them are writing the OPERATOR'S repo
// (this one, gen-toc) versus a build artifact or a temp file — a distinction none
// of the three cases so far has had to draw.
//
// So: rendering lives in this package, the os.WriteFile lives in cmd/llz, and
// TestPackageContainsNoWritePath fails if that ever stops being true.
//
// WHY `transition` AND NOT `gate`. It changes the repo — a second run after a rank
// change produces a different workflow. `--check` mode reports without writing,
// but a mode that happens not to mutate does not make the binding an observation.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "promote-pipeline",
		Short:  "generate the promotion workflow from each deployment's promotion_rank",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			State:  extension.Promoted,
			Grants: []extension.Grant{extension.ReadRepo},
		}},
	}
}
