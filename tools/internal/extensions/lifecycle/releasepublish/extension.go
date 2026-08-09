package releasepublish

// extension.go — `release-publish` declares itself, and declares that HALF OF IT
// HAS NOWHERE TO ATTACH.
//
// THIRTY-NINTH EXTENSION, TENTH CATALOG CORRECTION, AND THE THIRD FILE PULLED OUT
// OF THIS ONE ROW. `release-publish` is annotated "Template-repo-side, not
// instance-side" and lists five files. Three of them are not:
//
//   - `deliver-docs` (28th) — runs inside a rendered instance at copier time.
//   - `chart-publish` (31st) — scans a rendered instance's apl-values pins.
//   - `pin-instance-images` (here) — sets the throwaway INSTANCE repo's TF_IMAGE
//     and KUBE_IMAGE variables; e2e-instantiate.yml runs it with --instance.
//
// The row's own note is wrong about the majority of its contents, and the cause is
// the one this campaign has now seen three times: A ROW GROUPED BY WHEN THINGS RUN
// RATHER THAN BY WHAT THEY TOUCH. Everything here happens around a release, so
// everything here was filed as release machinery.
//
// AND THE NOTE IS RIGHT ABOUT `publish-charts`, WHICH IS THE INTERESTING PART.
// It packages every first-party Helm chart, pushes and keyless-cosign-signs it to
// an OCI registry, and runs in EXACTLY ONE PLACE: the template repo's own
// publish-charts.yml. It never runs in an instance. It has no instance lifecycle
// state, because it is not part of an instance's life — it produces the artifacts
// that every instance later consumes.
//
// SO IT GETS NO BINDING, and Incomplete says why. That is a different shape from
// every earlier use of that field: `reconcile-actions` and `template-sustain` mark
// code that HAS NOT MOVED, and `guard-manifests` marks a lane that could not.
// This marks moved code that is IN THE CATALOG AND IS NOT AN EXTENSION — which is
// worth recording, because the catalog's 57 rows have been treated throughout as a
// list of extensions-in-waiting, and at least one of them is not one.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `release-publish` declaration.
//
//	transition:configured "pin-images" [read-repo, cloud-mutate]
//
// PIN-INSTANCE-IMAGES POINTS AN INSTANCE AT THE IMAGES BUILT FROM THIS COMMIT, so
// the baked `llz` binary cannot drift from the workflow YAML rendered at the same
// commit — the recurring "llz: unknown flag" / stale-binary e2e failures. It sets
// GitHub repository VARIABLES on the instance repo and, with --build-if-missing,
// dispatches build-images.yml and waits.
//
// `cloud-mutate` at `configured` is the row `chart-publish` widened eight
// extractions ago, and this is its third shipping case. The argument holds
// unchanged: `configured` means "its inputs resolve", GitHub is configured before
// Linode is provisioned, and resolving an input can require CREATING it. An
// instance pointed at an image tag that was never built has an input that does not
// resolve until something builds it.
//
// A TRANSITION, NOT AN ASSERTION: it writes repository variables, and an assertion
// may hold read grants only. Unlike the six forced spellings this campaign
// recorded, nothing is forced here — pinning an image IS an act that moves the
// instance toward a state, not an observation wearing a write grant.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "release-publish",
		Short:  "point an instance at the images built from this commit, so its binary cannot drift from its workflows",
		Always: false,
		Bindings: []extension.Binding{{
			Kind:   extension.Transition,
			Name:   "pin-images",
			State:  extension.Configured,
			Grants: []extension.Grant{extension.ReadRepo, extension.CloudMutate},
		}},
		Incomplete: []string{
			"publish-charts has NO BINDING, and not because it has not moved — it is here. " +
				"It packages and cosign-signs the first-party Helm charts to an OCI registry " +
				"and runs in exactly one place: the template repo's own publish-charts.yml. It " +
				"never runs in an instance, so it has no instance lifecycle state to attach to. " +
				"It produces the artifacts every instance consumes rather than being part of any " +
				"instance's life. The catalog lists it as an extension candidate; on this " +
				"evidence it is template machinery, the same class as .template-manifest.",
		},
	}
}

// repoBinding is the binding this package reads the tree through. Every binding
// here declares read-repo and nothing that differs, so no choice can widen
// anything; it is looked up rather than reconstructed all the same, following
// objenc's seedBinding.
func repoBinding() extension.Binding {
	for _, b := range Extension().Bindings {
		for _, g := range b.Grants {
			if g == extension.ReadRepo {
				return b
			}
		}
	}
	panic("release-publish: no binding declares read-repo — the tree is read through one, so " +
		"its absence is a wiring bug")
}
