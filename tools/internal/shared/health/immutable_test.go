package health

import "testing"

// The StatefulSet refusal is the one this predicate was written from — it is
// what a WAL claim template added to a live loki-ingester actually produces.
func TestTheStatefulSetRefusalIsRecognised(t *testing.T) {
	const real = `The StatefulSet "loki-ingester" is invalid: spec: Forbidden: updates to statefulset ` +
		`spec for fields other than 'replicas', 'ordinals', 'template', 'updateStrategy', ` +
		`'revisionHistoryLimit', 'persistentVolumeClaimRetentionPolicy' and 'minReadySeconds' are forbidden`
	if !IsImmutableFieldRejection(real) {
		t.Fatal("the StatefulSet whole-spec refusal must classify as immutability — it is the " +
			"rejection this predicate exists for")
	}
}

func TestTheGenericValidationPhrasingsAreRecognised(t *testing.T) {
	for _, msg := range []string{
		`Service "harbor" is invalid: spec.clusterIP: Invalid value: "": field is immutable`,
		`Job.batch "x" is invalid: spec.template: Invalid value: ...: field is immutable`,
		`StorageClass parameters may not be changed after creation`,
		`spec.storageClassName: Forbidden: updates to pvc spec are forbidden`,
	} {
		if !IsImmutableFieldRejection(msg) {
			t.Errorf("not classified as immutability: %s", msg)
		}
	}
}

// The exclusions matter more than the inclusions: every one of these would send
// an operator to recreate a live object over something a recreate cannot fix.
func TestOtherRefusalsAreNotMistakenForImmutability(t *testing.T) {
	for _, msg := range []string{
		"",
		`Error from server (NotFound): statefulsets.apps "loki-ingester" not found`,
		`error validating data: ValidationError(StatefulSet.spec): unknown field "volumeClaimTemplate"`,
		`Error from server (Forbidden): statefulsets.apps "loki-ingester" is forbidden: ` +
			`User "system:serviceaccount:ci:llz" cannot patch resource`,
		`Error from server (Conflict): Operation cannot be fulfilled on statefulsets.apps "loki-ingester"`,
		`admission webhook "validate.kyverno.svc-fail" denied the request: field is immutable`,
	} {
		if IsImmutableFieldRejection(msg) {
			t.Errorf("wrongly classified as immutability: %s", msg)
		}
	}
}
