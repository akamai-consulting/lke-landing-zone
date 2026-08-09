package kyverno

// extension.go — `kyverno-policies` declares itself.
//
// THIRTY-SEVENTH EXTENSION. The catalog's note on this row is one line — "Writes
// policy — cluster-write, so not a check" — and it is the first row whose note
// turned out to be exactly right, first time, with nothing to correct. After eight
// corrections that is worth recording in the other direction.
//
// TWO CONSTRAINTS SHAPED THE MOVE, AND NEITHER WAS ABOUT THE DECLARATION.
//
// `//go:embed` PINS DATA TO ITS PACKAGE DIRECTORY. The policy manifests could not
// follow this package: the embedder takes three files from its own directory, and
// Go's embed cannot reach outside it. Moving only the kyverno-* subset would split
// one directory of related policy assets across two packages for the convenience
// of one test. So the manifests stay with the embedder and this test reaches them
// through a named const. Same family of constraint as internal/keycloak, where the
// language decided the order of the work rather than the design did.
//
// THE EMBEDDER HAS SINCE MOVED, and the const moved with it: the manifests were in
// tools/cmd/llz/manifests and are now in tools/internal/extensions/lifecycle/bootstrapcluster/manifests,
// because `bootstrap-cluster` was extracted. The constraint is unchanged — the
// assets still live beside whoever embeds them, and this package still cannot own
// them. What DID change is that they are no longer stranded in `package main`.
// TestManifestsStayWithTheEmbeddingPackage is what caught the move; it is doing
// exactly the job it was written for.
//
// `warn` WAS COPIED, NOT EXPORTED. It is a two-line ::warning:: printer that
// happened to live in this file and is used by two other package main files.
// Exporting it would put a symbol in this package's API whose only job is to be
// reachable from the other side. Printers and fixtures travel by copy — the same
// call made for firstNonEmpty, orAll and report.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `kyverno-policies` declaration.
//
//	transition:converged[cluster-read, cluster-write]
//
// It server-side applies a ClusterPolicy and then polls until Kyverno reports it
// Ready — so it both writes and reads, and the read is not incidental: an applied
// policy that never becomes Ready is a policy that is not enforcing, which is the
// failure mode the readiness wait exists to catch.
//
// A TRANSITION WITHOUT ARGUMENT, and that is the notable thing about it. Six
// extractions in a row have had to spell a CHECK as `transition:converged` because
// the vocabulary had nowhere else to put a mutating observation, each one true but
// forced. This one is a transition because it genuinely acts: applying admission
// policy is a step that moves the platform toward `converged`, not an observation
// wearing a write grant. The contrast is the useful part — it shows the forced
// spellings were forced, rather than the model simply calling everything a
// transition.
//
// WEBHOOK-RACE SOFT-FAIL is why this needs `cluster-read` beyond the readiness
// poll. Kyverno's own admission webhook can reject the apply while Kyverno itself
// is still starting; the command recognises that specific rejection and retries
// rather than failing the converge. health.IsWebhookRace is exported for exactly one
// other caller — the Keycloak gateway alias apply, which hits the identical race.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "kyverno-policies",
		Short:  "apply a Kyverno ClusterPolicy and wait until it is actually enforcing",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:  extension.Transition,
			State: extension.Converged,
			// `read-repo` IS A CORRECTION. policyName() reads the policy manifest
			// off disk to pull metadata.name out of it, and the binding claimed
			// only the two cluster grants — so the declaration said this never
			// touched the tree, and it did. Found by censusing which packages read
			// files against which declare it.
			Grants: []extension.Grant{extension.ReadRepo, extension.ClusterRead, extension.ClusterWrite},
		}},
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
	panic("kyverno-policies: no binding declares read-repo — the tree is read through one, so " +
		"its absence is a wiring bug")
}
