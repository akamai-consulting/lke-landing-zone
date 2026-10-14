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
// THREE CALLERS, AND THE THIRD IS THE DESTRUCTIVE ONE.
//
//	assert-overlay-applied   RUNTIME. Decides whether an undelivered overlay value
//	                         is undelivered because Argo has not got to it yet or
//	                         because the apiserver will never accept it. A verdict
//	                         of "immutable" is what prints the
//	                         `llz ci brownfield-migrate` remedy, so misclassifying
//	                         one costs an operator the next step.
//	assert-overlay-appliability  PR TIME. Grades a probe's refusal against what the
//	                         field map claims.
//	brownfield.createOnlyStillHolds  RUNTIME, and it ORPHAN-DELETES A LIVE OBJECT.
//	                         It re-asks the CreateOnly question of the real object
//	                         immediately before the delete and refuses unless the
//	                         answer here is an immutability refusal.
//
// AN EARLIER HEADER SAID "TWO CALLERS, AND NEITHER IS THE MIGRATION", on the
// reasoning that the migration's precondition is the field map's flag plus what
// the object carries rather than a live refusal. That was true when written and
// is not now: the migration gained a live probe precisely so a hand-set flag
// could not delete a StatefulSet unchecked, and this predicate is what grades it.
// A false positive here no longer costs an operator a remedy line — it clears an
// irreversible delete.
//
// It stays here, in the library where the "what is the cluster telling us"
// predicates live, because the next caller of it is the one that would otherwise
// write its own: a second, shorter list of Forbidden-shaped strings is exactly
// how IsTransientFetchError's neighbours came to be collected in one place.

import "strings"

// immutableMarkers are the ways the API server says "this field is fixed".
//
// CAPTURED, NOT REMEMBERED — and the difference was load-bearing. This list's
// previous comment claimed the markers were "transcribed from real rejections" and
// attributed Service clusterIP and PVC to the generic `field is immutable`
// message. Neither is true. Captured verbatim from a v1.34.8 apiserver:
//
//	StatefulSet spec   The StatefulSet "…" is invalid: spec: Forbidden: updates to
//	                   statefulset spec for fields other than 'replicas', … are forbidden
//	StorageClass       …: parameters: Forbidden: updates to parameters are forbidden.
//	Job completions    …: spec.completions: Invalid value: 3: field is immutable
//	PVC spec           The PersistentVolumeClaim "…" is invalid: spec: Forbidden: spec is
//	                   immutable after creation except resources.requests and
//	                   volumeAttributesClassName for bound claims
//	Service clusterIP  …: spec.clusterIPs[0]: Invalid value: ["…"]: may not change once set
//
// The PVC refusal matches ONLY `is immutable after crea`, and the Service refusal
// matched NOTHING before `may not change once set` was added — so a real clusterIP
// rejection was being graded "not immutability", which in assert-overlay-applied
// drops the migration remedy and in the appliability lane fails the row as an
// unclassified refusal. The fixtures that were supposed to cover both were
// hand-written text no apiserver emits, which is why nothing caught it.
//
// TWO ENTRIES ARE SPECULATIVE, and are marked so deliberately. No captured message
// uses `may not be changed` — a v1.34.8 apiserver spells the StorageClass refusal
// `Forbidden: updates to parameters are forbidden`, so that wording belongs to an
// earlier release and is kept for instances still on one. `cannot be updated` is
// anticipated rather than observed: a CRD's x-kubernetes-validations rule or an
// operator's own validation may well phrase itself that way. The cost of a spare
// inclusion is bounded by the admission-denial exclusion below.
//
// A LATER ROUND MUST NOT "CONFIRM" EITHER FROM A FIXTURE WRITTEN TO MATCH IT —
// that is exactly how this list came to hold two unpinned entries and to miss a
// real Service refusal. speculativeMarkers in immutable_test.go is the one place
// that set is declared, and the tests refuse an entry that quietly stops being
// speculative.
//
// Anything outside this set is deliberately NOT classified as immutability: a
// malformed patch and an immutable field are both "the API said no", and reading
// the first as the second would send an operator to recreate an object over a typo.
var immutableMarkers = []string{
	"forbidden: updates to",   // StatefulSet/Deployment/StorageClass whole-spec refusal
	"field is immutable",      // the generic validation message (Job, Deployment selector)
	"is immutable after crea", // PersistentVolumeClaim's whole-spec refusal
	"may not change once set", // Service clusterIP, and every field validated that way
	"may not be changed",      // SPECULATIVE (legacy): see the note below
	"cannot be updated",       // SPECULATIVE (anticipated): see the note below
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
	// THE WARNINGS COME OFF, THEN THE WHOLE MESSAGE IS JUDGED AS ONE.
	//
	// A DENIAL IS NOT A LINE, IT IS A MESSAGE. An earlier version of this judged each
	// line independently, on the reasoning that only the line carrying the marker
	// mattered. That INVERTED the verdict for the shape admission controllers
	// actually emit: kubectl prints `admission webhook "…" denied the request:` and
	// then the policy's own body on the FOLLOWING lines, so the denial and its
	// reason are never on one line. Measured against a real Kyverno refusal whose
	// rule text says "cannot be updated" — per-line said IMMUTABLE, which grades a
	// policy-denied probe as "CREATE-ONLY, as declared" and certifies, from a policy
	// artifact, the hand-set boolean wired to a StatefulSet delete. Gatekeeper and
	// VAP have the same shape. The exclusion had been narrowed to single-line
	// denials, which is the rare form.
	//
	// SO WHAT IS ACTUALLY DROPPED IS THE WARNING. The bug the anchoring was written
	// for is a webhook `Warning:` about a DIFFERENT object riding along on the same
	// stderr and declassifying the refusal under it. A warning is not a refusal, it
	// announces itself as such, and kubectl always prefixes it literally. Removing
	// those lines and judging the remainder as one message fixes that case without
	// giving up the multi-line denial.
	var kept []string
	for _, line := range strings.Split(strings.ToLower(msg), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "warning:") {
			continue
		}
		kept = append(kept, line)
	}
	m := strings.Join(kept, "\n")
	if isAdmissionDenial(m) {
		return false
	}
	for _, p := range immutableMarkers {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}

// admissionDenialMarkers are how an admission webhook or a ValidatingAdmissionPolicy
// says it refused a request.
//
// A NAMED LIST BESIDE immutableMarkers, for the reason this file's header gives
// about second lists: as an unnamed literal inside the function it was
// untestable, undocumented and allocated per call, and the four entries this repo
// added to it went in with no test in either direction — each could be deleted
// with `go test ./...` green.
//
// AN ADMISSION DENIAL IS NOT AN IMMUTABILITY RULE. Both can phrase themselves with
// the words above — "may not be changed" and "cannot be updated" are natural
// things for a policy author to write — and a recreate helps with neither: the
// policy would refuse the new object too.
//
// THESE ARE READ OVER THE WHOLE MESSAGE (minus its Warning: lines), because a
// denial introduces itself on one line and gives its reason on the next.
//
// THE VAP SPELLINGS ARE HERE FOR COMPLETENESS, NOT FOR A LIVE HAZARD, and saying
// so keeps the next reader from defending the wrong thing. Neither VAP this repo
// ships emits any immutableMarker — llz-wave-health-guard says "is not a vetted
// health-safe kind", the PVC clone policy says "cannot apply the … tag" — and the
// apiserver runs ValidateUpdate BEFORE the admission chain, so an immutability
// refusal and a policy denial do not arrive in one response for one object. They
// are listed because a future policy could word itself that way, and because the
// cost of the exclusion is now bounded by the line anchoring above.
var admissionDenialMarkers = []string{
	"admission webhook",
	"admission policy",          // ValidatingAdmissionPolicy, as the apiserver names it
	"validatingadmissionpolicy", // and as a message may spell it
	"denied the request",        // the webhook phrasing
	"denied request",            // the VAP phrasing
}

// isAdmissionDenial reports whether a message is an admission refusal. Takes
// ALREADY-LOWERCASED text: the caller lowercases once.
func isAdmissionDenial(msg string) bool {
	for _, d := range admissionDenialMarkers {
		if strings.Contains(msg, d) {
			return true
		}
	}
	return false
}
