package health

import (
	"strings"
	"testing"
)

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

// capturedRefusals are verbatim apiserver output, taken from a kind v1.34.8
// cluster — the same version the appliability lane runs against.
//
// THE PREVIOUS FIXTURES WERE INVENTED, and that is how two markers came to be
// unpinned and one real refusal came to be unclassified. `Service "harbor" …
// field is immutable` and `spec.storageClassName: Forbidden: updates to pvc spec
// are forbidden` are not things an apiserver says: the real Service message is
// `may not change once set` and the real PVC message is `spec is immutable after
// creation except …`. A fixture written to match the classifier tests the
// classifier against itself.
var capturedRefusals = map[string]string{
	"StatefulSet whole-spec": `The StatefulSet "loki-ingester" is invalid: spec: Forbidden: updates to ` +
		`statefulset spec for fields other than 'replicas', 'ordinals', 'template', 'updateStrategy', ` +
		`'persistentVolumeClaimRetentionPolicy' and 'minReadySeconds' are forbidden`,
	"StorageClass parameters": `The StorageClass "standard" is invalid: parameters: Forbidden: ` +
		`updates to parameters are forbidden.`,
	"StorageClass provisioner": `The StorageClass "standard" is invalid: provisioner: Forbidden: ` +
		`updates to provisioner are forbidden.`,
	"Job completions": `The Job "capjob" is invalid: spec.completions: Invalid value: 3: field is immutable`,
	"PVC whole-spec": `The PersistentVolumeClaim "capp" is invalid: spec: Forbidden: spec is immutable ` +
		`after creation except resources.requests and volumeAttributesClassName for bound claims`,
	"Service clusterIP": `The Service "capsvc" is invalid: spec.clusterIPs[0]: Invalid value: ` +
		`["10.96.0.98"]: may not change once set`,
}

func TestEveryCapturedRefusalIsRecognised(t *testing.T) {
	for name, msg := range capturedRefusals {
		if !IsImmutableFieldRejection(msg) {
			t.Errorf("%s is not classified as immutability, so an operator meeting it is told to "+
				"keep waiting for something that will never apply:\n%s", name, msg)
		}
	}
}

// speculativeMarkers are the entries no captured refusal carries. Declared in ONE
// place, because a second copy is how the two tests below would come to disagree
// about which entries are actually pinned.
//
// BOTH ARE LEGACY OR ANTICIPATED PHRASINGS. A v1.34.8 apiserver spells the
// StorageClass refusal `Forbidden: updates to parameters are forbidden` — the
// `may not be changed` wording the old fixture used belongs to an earlier release,
// and is kept for instances still on one. `cannot be updated` is anticipated: a
// CRD's x-kubernetes-validations rule or an operator's own validation may phrase
// itself that way. Neither may be "confirmed" from a fixture written to match it.
var speculativeMarkers = map[string]bool{
	"cannot be updated":  true,
	"may not be changed": true,
}

func TestEveryImmutableMarkerIsCarriedByACapturedRefusal(t *testing.T) {
	// A marker no real message uses cannot be verified from this file, and the risk
	// is that a later round "confirms" it from a fixture written to match it. The
	// list may hold at most one such entry, and it has to be declared here rather
	// than discovered — see the note on `cannot be updated` in immutable.go.
	for _, m := range immutableMarkers {
		var carried bool
		for _, msg := range capturedRefusals {
			if strings.Contains(strings.ToLower(msg), m) {
				carried = true
			}
		}
		if !carried && !speculativeMarkers[m] {
			t.Errorf("immutableMarkers entry %q is carried by no CAPTURED apiserver refusal. Either "+
				"capture the message that needs it, or mark it speculative in the same breath as "+
				"saying why it is kept — an unpinned marker is one a later edit deletes silently", m)
		}
		if carried && speculativeMarkers[m] {
			t.Errorf("%q is marked speculative but a captured refusal now carries it — drop it from "+
				"the speculative set so the entry is genuinely pinned", m)
		}
	}
}

func TestEachNonSpeculativeMarkerIsIndividuallyLoadBearing(t *testing.T) {
	// The check that actually catches a deletion. Before this file, `cannot be
	// updated` and `is immutable after crea` could each be removed with the whole
	// tree green — and the second is the ONLY marker matching a real PVC refusal.
	for _, m := range immutableMarkers {
		var sole []string
		for name, msg := range capturedRefusals {
			low := strings.ToLower(msg)
			if !strings.Contains(low, m) {
				continue
			}
			only := true
			for _, other := range immutableMarkers {
				if other != m && strings.Contains(low, other) {
					only = false
				}
			}
			if only {
				sole = append(sole, name)
			}
		}
		if len(sole) == 0 && !speculativeMarkers[m] {
			t.Errorf("no captured refusal needs %q ALONE, so deleting it changes no verdict this "+
				"file can see", m)
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
