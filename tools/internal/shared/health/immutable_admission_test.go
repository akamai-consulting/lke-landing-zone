package health

// immutable_admission_test.go pins the admission-denial exclusions, in both
// directions, one entry at a time.
//
// EVERY ENTRY WAS UNPINNED. Deleting any single marker from
// admissionDenialMarkers left `go test ./...` green across the tree — including
// the two that predate this file, which survived only because the one existing
// fixture happened to contain both spellings at once. A list whose entries cannot
// be individually removed-and-noticed is a list that will be edited by accident.
//
// AND THE MULTI-LINE HANDLING, in both directions, because the two obvious
// implementations are each wrong in one of them. Judging the whole blob lets a
// webhook `Warning:` about a DIFFERENT object declassify the refusal underneath it
// (which in assert-overlay-applied drops the `llz ci brownfield-migrate` remedy).
// Judging each line independently INVERTS the common case instead: a real denial
// puts its header on one line and its reason on the next, so no line carries both
// and the reason line reads as a bare immutability marker. What is actually right
// is to drop the Warning: lines and judge the rest as one message — and only a
// suite carrying both shapes can tell the three implementations apart.

import "testing"

func TestEachAdmissionDenialMarkerIsIndividuallyLoadBearing(t *testing.T) {
	// One marker per case, each in a message that ALSO carries an immutability
	// marker — because that is the only situation where the exclusion changes an
	// answer. Remove the marker under test and the message classifies as
	// immutability; that is what makes the entry load-bearing.
	for _, tc := range []struct{ marker, msg string }{
		{"admission webhook", `Error from server: admission webhook "vpol.example.com" denied it: field is immutable`},
		{"admission policy", `Error from server: validating admission policy "llz-guard" refused: cannot be updated`},
		{"validatingadmissionpolicy", `ValidatingAdmissionPolicy 'llz-guard' refused: this may not be changed`},
		{"denied the request", `admission plugin denied the request: the label cannot be updated`},
		{"denied request", `policy 'llz-guard' denied request: this field is immutable`},
	} {
		t.Run(tc.marker, func(t *testing.T) {
			if IsImmutableFieldRejection(tc.msg) {
				t.Errorf("an admission denial was classified as immutability.\nmessage: %s\n"+
					"A recreate does not help with a policy refusal — the policy would refuse the new "+
					"object too — and in assert-overlay-applied this prints a brownfield migration as "+
					"the remedy for something no migration fixes", tc.msg)
			}
			// The control: the SAME sentence without the denial wording must still read as
			// immutability, or this case would pass because the message is unclassifiable
			// rather than because the marker did its job.
			bare := "the field is immutable"
			if !IsImmutableFieldRejection(bare) {
				t.Fatalf("the control message %q no longer classifies as immutability, so this case "+
					"proves nothing about %q", bare, tc.marker)
			}
		})
	}
}

func TestAMultiLineAdmissionDenialIsNotImmutability(t *testing.T) {
	// THE SHAPE ADMISSION CONTROLLERS ACTUALLY EMIT, and the one a per-line reading
	// got backwards. kubectl prints the denial header, then the policy's own body on
	// the FOLLOWING lines, so the marker and the denial are never on one line. Judging
	// lines independently classified all three of these as immutability — which grades
	// a policy-denied probe "CREATE-ONLY, as declared" and certifies the hand-set
	// boolean that deletes a StatefulSet. Every case in the table above is single-line;
	// without these, the exclusion is pinned only in the form that is rare in practice.
	for _, tc := range []struct{ name, msg string }{
		{"kyverno", `Error from server: admission webhook "validate.kyverno.svc-fail" denied the request:

resource StatefulSet/monitoring/loki-ingester was blocked due to the following policies

require-storage-immutability:
  autogen-check-sc: 'validation error: storageClassName cannot be updated.
    rule autogen-check-sc failed'`},
		{"gatekeeper", `Error from server: admission webhook "validation.gatekeeper.sh" denied the request:
[storage-immutability] spec.volumeClaimTemplates may not be changed on an existing workload`},
		{"vap", `Error from server: ValidatingAdmissionPolicy 'llz-storage-guard' with binding 'llz-storage-guard-binding' denied request:
the storage class of a bound claim cannot be updated`},
		{"crlf", "Error from server: admission webhook \"x.example.com\" denied the request:\r\nspec.foo is immutable after creation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if IsImmutableFieldRejection(tc.msg) {
				t.Errorf("a multi-line admission denial was classified as immutability:\n%s", tc.msg)
			}
		})
	}
}

func TestAnAdmissionWarningDoesNotDeclassifyTheRefusalUnderIt(t *testing.T) {
	// MEASURED. kubectl returns the whole stderr, and a webhook's Warning about a
	// sibling object rides along with the validation failure. Matched across the
	// whole text, the warning excluded the refusal below it.
	msg := `Warning: the admission webhook "mutate.kyverno.svc-ignore" denied the request for a sibling object
The StatefulSet "loki-ingester" is invalid: spec: Forbidden: updates to statefulset spec for fields other than 'replicas', 'ordinals', 'template' are forbidden`
	if !IsImmutableFieldRejection(msg) {
		t.Error("an unrelated admission warning declassified a genuine immutability refusal. A " +
			"Warning: is not a refusal and says so; it comes off before the message is judged")
	}
}

func TestADenialAndAMarkerOnTheSameLineIsStillNotImmutability(t *testing.T) {
	// The converse of the test above, so dropping the Warning: lines cannot be
	// "simplified" into dropping the exclusion altogether.
	msg := `Error from server: admission webhook "x.example.com" denied the request: spec.foo is immutable after creation`
	if IsImmutableFieldRejection(msg) {
		t.Error("a denial that phrases itself with an immutability marker on its own line was read " +
			"as an apiserver immutability rule")
	}
}

func TestARealStatefulSetRefusalStillClassifies(t *testing.T) {
	// The anchor case for the whole file: the refusal this predicate was written
	// from. If the message handling ever breaks it, everything above is decoration.
	msg := `The StatefulSet "loki-ingester" is invalid: spec: Forbidden: updates to statefulset spec for fields other than 'replicas', 'ordinals', 'template', 'updateStrategy', 'persistentVolumeClaimRetentionPolicy' and 'minReadySeconds' are forbidden`
	if !IsImmutableFieldRejection(msg) {
		t.Fatal("the real StatefulSet whole-spec refusal no longer classifies as immutability")
	}
}
