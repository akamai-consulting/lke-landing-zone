package argodiag_test

// THE FIFTH BINDING KIND IS REFUSED, AND THIS IS WHAT THE REFUSAL RESTS ON.
//
// `argocd-diagnostics` opened the case for a `diagnostic` kind and named two
// others as the cases it should be argued from. Both shipped, and both refuted it:
//
//   - `phase-timing` attaches to NO state (its subject is the run), so it and this
//     command disagree about the one thing a binding encodes.
//   - `doctor-probes` was the explicit tiebreaker. phase-timing wrote down what
//     its verdict would mean IN ADVANCE: if it attaches to a state, "the family
//     splits three ways and 'diagnostic' was never a kind — it was a description
//     of a tone of voice." It attached to `configured`, as a plain assertion, and
//     needed no Incomplete note.
//
// A verdict resting on a prediction that resolved is only as durable as the
// evidence, and nothing else watches it. If doctor-probes ever grows an Incomplete
// note or stops attaching to a state, the argument in this package's header stops
// being true and the fifth kind is open again — which a reader of that header
// would have no way to discover. So the evidence is pinned here, beside the claim
// it supports, rather than left to be re-derived by whoever next wonders.
//
// This is the same lesson the campaign has now learned twice: a note recording why
// something is blocked ages badly, because the reason expires before anyone
// re-reads it. Four stale Incomplete markers were found that way. The fix is not
// to write better notes, it is to make the note's premise fail loudly.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/argodiag"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/doctor"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/phasetiming"
)

func TestTheDiagnosticFamilySplitsThreeWays(t *testing.T) {
	// Case three, the tiebreaker. Both halves matter: attaching to a real state is
	// what proved a diagnostic CAN be placed, and needing no note is what proved
	// the placement was a fit rather than another approximation.
	probes := doctor.Extension()
	if len(probes.Incomplete) != 0 {
		t.Errorf("doctor-probes grew an Incomplete note (%v) — it was the case that settled "+
			"the fifth-kind question by NOT needing one; re-read argodiag's header, because "+
			"its verdict rests on this", probes.Incomplete)
	}
	var placed bool
	for _, b := range probes.Bindings {
		if b.State == extension.Configured && b.Kind == extension.Assertion {
			placed = true
		}
	}
	if !placed {
		t.Errorf("doctor-probes no longer holds assertion:configured (has %v) — that placement "+
			"is the evidence that 'read something and print it' is not one lifecycle "+
			"position, and the refusal of a `diagnostic` kind depends on it", probes.Bindings)
	}

	// Case two. The disagreement between this and argodiag is the other half: one
	// attaches to the failure of `converged`, one to nothing at all.
	if len(phasetiming.Extension().Incomplete) == 0 {
		t.Error("phase-timing's Incomplete note is gone — it records that this extension's " +
			"binding is a placement rather than a fit, which is half the reason no fifth " +
			"kind was invented")
	}

	// And case one still says so itself. The note is the disposition; if it is
	// emptied, the mislabelled binding is left with nothing explaining it.
	notes := strings.Join(argodiag.Extension().Incomplete, " ")
	if !strings.Contains(notes, "REFUTED") {
		t.Error("argocd-diagnostics' Incomplete note no longer records that the fifth kind was " +
			"refuted — the binding is still deliberately wrong and the note is what says so")
	}
}

// A `diagnostic` kind must not appear without this file being revisited. Pinned by
// BEHAVIOUR rather than by reading the model's kind list, which is unexported —
// exporting it to satisfy a test would be exporting something no caller needs, and
// asking the validator is the stronger check anyway: it fails if the word is
// accepted, however it got there.
func TestDiagnosticIsNotABindingKind(t *testing.T) {
	probe := extension.Extension{
		Name: "probe", Short: "x",
		Bindings: []extension.Binding{{
			Kind: extension.BindingKind("diagnostic"), State: extension.Converged,
			Grants: []extension.Grant{extension.ClusterRead},
		}},
	}
	if errs := probe.Validate(); len(errs) == 0 {
		t.Error("`diagnostic` is now a valid binding kind — three shipping cases said it " +
			"describes a tone of voice rather than a lifecycle position; see this " +
			"package's header before keeping it")
	}
}
