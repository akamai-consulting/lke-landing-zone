package argodiag_test

// THE FIFTH BINDING KIND IS REFUSED, AND THE REFUSAL IS NOW STRUCTURAL.
//
// `argocd-diagnostics` opened the case for a `diagnostic` kind and named two
// others to argue it from. Both shipped, and both refuted it: `phase-timing`
// attaches to NO state (its subject is the run), so it and this command disagreed
// about the one thing a binding encodes; `doctor-probes` was the tiebreaker and
// attached to `configured` as a plain assertion, needing no note at all. A kind
// wide enough for all three would have had to mean "produces operator-facing
// output and never fails", which is a property of the OUTPUT rather than a
// position in the lifecycle — and position is the entire content of a binding.
//
// WHAT CHANGED, AND WHY THIS FILE IS SHORTER. That verdict used to be pinned by
// reading three declarations and asserting their shape: doctor-probes still has no
// Incomplete note, phase-timing still has one, argodiag's still says REFUTED. All
// three declarations are now GONE, because all three packages moved to
// internal/verbs — they are cobra commands, not capabilities, and nobody enables
// or disables `llz doctor`.
//
// That is the same verdict reached one level up. The old question was "which of
// the four kinds does a diagnostic hold?"; the answer is that it holds none,
// because it is not an extension. Two of the three carried Incomplete notes saying
// exactly that in their own words — "the binding kind is wrong: this is a
// DIAGNOSTIC" and "the binding is a placement, not a fit" — and moving them is
// what discharges those notes rather than restating them.
//
// So the evidence this file pins is no longer three declarations' shapes. It is
// the absence of any declaration in internal/verbs at all, which
// TestVerbsDoNotDeclareExtensions (internal/shared/extension) asserts for the whole
// tree. What stays here is the one claim that is specific to this model: the word
// `diagnostic` must not become a binding kind without someone reading the above.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

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
			"describes a tone of voice rather than a lifecycle position, and all three have " +
			"since left the extension model entirely; see this package's header before keeping it")
	}
}
