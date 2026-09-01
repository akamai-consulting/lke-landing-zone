package health

// immutable.go — is a rejected write a rejection of the CHANGE, or of the field
// being changeable at all?
//
// It sits beside IsTransientFetchError and IsGitAuthError for the reason their
// header gives: all of these answer "what is the cluster actually telling us",
// none of them acts, and getting one wrong turns a permanent condition into a
// retryable one or the reverse. This is the permanent end of that scale — no
// amount of polling makes a create-time field mutable, and a gate that reads such
// a refusal as a transient burns its whole budget on a question already answered.
//
// ONE CALLER TODAY, AND THE HEADER USED TO CLAIM TWO. `llz ci
// assert-overlay-applied` uses it to decide whether an undelivered overlay value
// is undelivered because Argo has not got to it yet or because the apiserver will
// never accept it. The brownfield migration does NOT: its precondition is the
// field map's CreateOnly flag plus what the object actually carries, never a live
// refusal — so saying the two "must not disagree" described a coupling that does
// not exist.
//
// It stays here, in the library where the "what is the cluster telling us"
// predicates live, because the next caller of it is the one that would otherwise
// write its own: a second, shorter list of Forbidden-shaped strings is exactly
// how IsTransientFetchError's neighbours came to be collected in one place.

import "strings"

// immutableMarkers are the ways the API server says "this field is fixed".
//
// TRANSCRIBED FROM REAL REJECTIONS, not invented. The first is a StatefulSet's
// (the whole-spec refusal that names the fields it WILL take), the second and
// third are the generic validation messages used for Service clusterIP, Job spec,
// PVC and StorageClass parameters, and the fourth is what a narrowed field
// rejection reads like. Anything outside this set is deliberately NOT classified
// as immutability: a malformed patch and an immutable field are both "the API
// said no", and reading the first as the second would send an operator to
// recreate an object over a typo.
var immutableMarkers = []string{
	"forbidden: updates to",   // StatefulSet/Deployment whole-spec refusal
	"field is immutable",      // the generic validation message
	"may not be changed",      // ditto, older phrasing
	"cannot be updated",       // narrowed field rejections
	"is immutable after crea", // "immutable after creation"
}

// IsImmutableFieldRejection reports whether an apiserver refusal is about a field
// that cannot change on an existing object — as opposed to a malformed request, a
// conflict, an admission denial, or a permission fault.
//
// Empty is false: no message is not evidence of anything, and the caller that
// treats "" as immutability would recreate objects on an unreadable error.
func IsImmutableFieldRejection(msg string) bool {
	if msg == "" {
		return false
	}
	m := strings.ToLower(msg)
	// An admission webhook can phrase its own denial with any of the words above.
	// It is not the API server's immutability rule and a recreate would not help,
	// so it is excluded before the markers are read.
	if strings.Contains(m, "admission webhook") {
		return false
	}
	for _, p := range immutableMarkers {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}
