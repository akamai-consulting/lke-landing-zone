package phasetiming

// extension.go — `phase-timing` declares itself, and settles a question by
// failing to answer it.
//
// THIRTY-FIFTH EXTENSION, AND THE SECOND DIAGNOSTIC. `argocd-diagnostics` was
// case one for a missing fifth binding kind: it reads a failing platform, prints
// it for a human, and always exits 0, so no kind fits. It shipped declared
// `assertion:converged` with an Incomplete note, on the rule that a new word needs
// a declaration to be IMPOSSIBLE and two independent shipping cases. The note named
// `phase-timing` and `doctor-probes` as the likely cases two and three, and posed
// the shape question a fifth kind would have to answer:
//
//	does a diagnostic attach to a STATE at all, or to the FAILURE of one?
//
// THE ANSWER IS THAT THE TWO CASES DISAGREE, and that is this extraction's
// finding — a negative result, and the most useful thing it produced.
//
//   - `argocd-diagnostics` attaches to the FAILURE of `converged`. It is the thing
//     you run after convergence did not happen, to find out why.
//   - `phase-timing` attaches to NO state. It records the boundaries BETWEEN
//     states — `phase-mark` is called at each transition and `phase-report` reads
//     the whole timeline at the end. Its subject is the RUN, not the platform.
//
// Those are not one kind. A `Diagnostic` binding wide enough to hold both would
// mean "produces operator-facing output and never fails", which describes a
// property of the OUTPUT rather than a position in the lifecycle — and position in
// the lifecycle is the entire content of a binding. Merging them would put a word
// in the vocabulary that says nothing about where the thing attaches, which is the
// one question the model exists to answer.
//
// SO STILL NOTHING IS INVENTED, and now for a better reason than "not enough
// cases". Two cases arrived, they met the bar for COUNT, and they failed the bar
// for SHAPE. `doctor-probes` is the remaining candidate and is worth extracting for
// this alone: if it attaches to a state (it probes whether credentials resolve,
// which is `configured`), then the family splits three ways and "diagnostic" was
// never a kind — it was a description of a tone of voice.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"

// Extension is the `phase-timing` declaration.
//
//	assertion:operating[cluster-read]   — see the file header; this is a placement,
//	                                      not a fit
//
// `cluster-read` is exactly true and is the only part of this that is. The
// image-pulls lane reads the cluster's `Pulled` Events to attribute per-image pull
// durations; nothing here writes to a cluster, a cloud, or the repo.
//
// The phase log itself is a WRITE, and deliberately needs no grant: it goes to
// $LLZ_PHASE_LOG under $RUNNER_TEMP, and `write-repo` was defined by `deliver-docs`
// as the instance repo's TRACKED files — a temp dir needs no grant, the same way
// reading /tmp needs no read-repo. This is the first extraction to lean on that
// boundary, and it holds.
//
// Everything else is placement. `operating` is where a reader is least likely to
// mistake it for a gate on progress, and `assertion` is the only kind that can hold
// a read grant at a state this thing has no opinion about. Incomplete says so.
func Extension() extension.Extension {
	return extension.Extension{
		Name:   "phase-timing",
		Short:  "record each phase boundary and report where a run's time went",
		Always: true,
		Bindings: []extension.Binding{{
			Kind:   extension.Assertion,
			State:  extension.Operating,
			Grants: []extension.Grant{extension.ClusterRead},
		}},
		Incomplete: []string{
			"the binding is a placement, not a fit. This is INSTRUMENTATION: it contributes no " +
				"verdict, cannot fail, and attaches to no single state — it records the " +
				"boundaries BETWEEN states, and its subject is the run rather than the platform. " +
				"It is the second diagnostic-family extension after argocd-diagnostics, and the " +
				"two DISAGREE about shape: that one attaches to the failure of `converged`, this " +
				"one to nothing. They are not one kind, which is why no fifth kind was invented " +
				"even though the two-case bar was met. THE TIEBREAKER NAMED IN THIS FILE'S " +
				"HEADER HAS SINCE SHIPPED: `doctor-probes` attaches to `configured` as a plain " +
				"assertion and needed no note, which is the outcome the header said would mean " +
				"'diagnostic' was never a kind. The placement stands; the question is closed.",
		},
	}
}
