package kubectlprobe

// notananswer_test.go pins the failures that must NOT read as "the object is
// absent", one entry at a time.
//
// THE LIST SHIPPED WITH SEVEN ENTRIES AND NO TESTS. Deleting the whole guard left
// 149 packages green, so every entry was individually unnoticed too — and a census
// against real kubectl then showed four of the seven changed no verdict for any
// message kubectl can emit, and a fifth was redundant with its pair-mate. A list
// nothing pins is a list nobody can tell is dead. This file is the same shape as
// health/immutable_admission_test.go, which was written in the same commit and
// should have been applied here first: it would have caught the dead entries at
// authoring time rather than in review.
//
// EVERY CASE IS REAL KUBECTL OUTPUT, not an invented phrase, because a fixture
// written to match the classifier tests the classifier against itself.

import (
	"errors"
	"strings"
	"testing"
)

// theseAreNotAnswers are messages where kubectl never reached an apiserver. Each
// one contains an absenceMarker — that is the whole point; without one the case
// would pass whether or not the exclusion exists.
var theseAreNotAnswers = []struct{ name, msg string }{
	{"kubectl is not installed", `exec: "kubectl": executable file not found in $PATH`},
	{"current-context names a context that is gone",
		`Error in configuration: context was not found for specified context: prod`},
	{"the kubeconfig itself will not parse",
		"Error in configuration: \n* unable to read client-cert /p/c.crt for u due to open /p/c.crt: no such file or directory\n* context was not found for specified context: prod"},
	// A `kubectl exec` that never started a process in the container. converge probes
	// exactly this shape (`kubectl exec … test -s <path>` on the OpenBao audit log),
	// and the relayed message carries the bare "not found" that absenceMarkers reads —
	// so without an exclusion, "the exec could not run" answers "the file is absent".
	// THE WORST ONE, and the shape this list was missing. client-go spells a missing
	// exec credential plugin "executable <NAME> not found" — never "executable file
	// not found" — so the bare "not found" in absenceMarkers won, and
	// `llz ci assert-overlay-applied` (which SHIPS to adopters) reported every mapped
	// object absent, printed green ticks and exited 0. A broken kubeconfig certified
	// as "this instance does not run that app".
	{"the exec credential plugin is not installed",
		"couldn't get current server API group list: Get \"https://10.0.0.1:6443/api?timeout=32s\": " +
			"getting credentials: exec: executable kubelogin not found\n\n" +
			"It looks like you are trying to use a client-go credential plugin that is not installed."},
	{"the container has no such binary",
		`command terminated with exit code 126: OCI runtime exec failed: exec failed: ` +
			`unable to start container process: exec: "test": executable file not found in $PATH: unknown`},
}

func TestAToolingFaultIsNotAnAbsentObject(t *testing.T) {
	for _, tc := range theseAreNotAnswers {
		t.Run(tc.name, func(t *testing.T) {
			// The control FIRST: if the message carried no absence marker, the case would
			// pass with the exclusion deleted and prove nothing at all. That is exactly how
			// four dead entries survived authoring.
			if !containsAnyAbsenceMarker(tc.msg) {
				t.Fatalf("this message carries no absenceMarker, so it would classify as Unknown "+
					"with or without the exclusion — the case proves nothing.\nmessage: %s", tc.msg)
			}
			if IsAbsentText(tc.msg) {
				t.Errorf("a failure that never reached an apiserver was classified as the object "+
					"being absent.\nmessage: %s\nA caller then states, as fact, that a resource does "+
					"not exist on a cluster it never contacted — and points the reader at whatever "+
					"was supposed to create it", tc.msg)
			}
			if ClassifyErr(errors.New(tc.msg)) != Unknown {
				t.Errorf("ClassifyErr did not report Unknown for %q", tc.msg)
			}
		})
	}
}

func TestEachNotAnAnswerMarkerIsIndividuallyLoadBearing(t *testing.T) {
	// A dead entry is not harmless: it is surface for a future message to collide
	// with, and it makes the list look like it is doing work. Every entry must be
	// the sole reason at least one real message is excluded.
	for _, marker := range notAnAnswerMarkers {
		var covered bool
		for _, tc := range theseAreNotAnswers {
			if !strings.Contains(strings.ToLower(tc.msg), marker) {
				continue
			}
			// Sole reason: no OTHER entry also matches this message.
			only := true
			for _, other := range notAnAnswerMarkers {
				if other != marker && strings.Contains(strings.ToLower(tc.msg), other) {
					only = false
				}
			}
			if only {
				covered = true
			}
		}
		if !covered {
			t.Errorf("no test message is excluded by %q ALONE, so deleting that entry would change "+
				"no verdict any test can see. Either it is dead — four entries in the first version "+
				"of this list were — or this file is missing the real kubectl message that needs it",
				marker)
		}
	}
}

func TestAGenuineAbsenceStillClassifies(t *testing.T) {
	// The anchor. If the exclusions ever swallow a real NotFound, every caller that
	// legitimately skips work on an absent object hard-fails instead.
	for _, msg := range []string{
		`Error from server (NotFound): statefulsets.apps "loki-ingester" not found`,
		`Error from server (NotFound): pods "openbao-0" not found`,
		`No resources found in monitoring namespace.`,
		`error: the server doesn't have a resource type "externalsecrets"`,
	} {
		if !IsAbsentText(msg) {
			t.Errorf("a genuine absence stopped classifying: %s", msg)
		}
	}
}

func TestAWarningLineDoesNotDeclassifyTheAbsenceUnderIt(t *testing.T) {
	// kubectl writes deprecation notices and admission Warning: lines to the same
	// stderr this reads. A warning that happens to carry an exclusion phrase must
	// not turn a real NotFound into "could not tell" — which would cost a retry
	// budget and, in the lanes that skip on absence, a hard failure instead.
	msg := "Warning: exec: \"kubectl\": executable file not found in $PATH was reported by a plugin\n" +
		`Error from server (NotFound): statefulsets.apps "loki-ingester" not found`
	if !IsAbsentText(msg) {
		t.Error("a Warning: line declassified a genuine NotFound beneath it")
	}
}

func TestTheExecExclusionIsAnchoredToKubectlNotToAnyRelayedRuntimeError(t *testing.T) {
	// `executable file not found` on its own is NOT unique to a client-side fault:
	// the container runtime emits it and the apiserver relays it verbatim. converge
	// probes exactly this shape with `kubectl exec … test -s <path>`, so an
	// unanchored entry would classify a real apiserver answer as "kubectl is
	// missing" — invisible today only because Exists collapses Absent and Unknown.
	relayed := `command terminated with exit code 126: OCI runtime exec failed: exec failed: ` +
		`unable to start container process: exec: "test": executable file not found in $PATH: unknown`
	for _, m := range notAnAnswerMarkers {
		if m == "oci runtime exec failed" {
			continue // the entry that is SUPPOSED to match this, asserted in the table above
		}
		if strings.Contains(strings.ToLower(relayed), m) {
			t.Errorf("notAnAnswerMarkers entry %q matches a message the APISERVER relays, not a "+
				"client-side fault. Anchor it to kubectl's own `exec: \"kubectl\":` prefix", m)
		}
	}
}

func containsAnyAbsenceMarker(msg string) bool {
	low := strings.ToLower(msg)
	for _, m := range absenceMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func TestProbeDetailCarriesWhatKubectlActuallySaid(t *testing.T) {
	// THE ARM WAS TESTED AND THE DIAGNOSIS WAS NOT. Deleting `detail = ErrText(err)`
	// left the whole tree green — and with it gone every caller printing
	// RefusalText(detail) says "(the apiserver said nothing)", so a dead cluster, a
	// missing exec plugin, a bad context and an RBAC denial go back to being
	// byte-identical. That is verbatim the regression ProbeDetail exists to fix.
	prev, prevRetries := Exec, Retries
	Retries = 1
	Exec = func(string, ...string) ([]byte, error) {
		return nil, errors.New(`Error from server (Forbidden): statefulsets.apps "loki-ingester" is ` +
			`forbidden: User "system:serviceaccount:ci:llz" cannot patch resource "statefulsets"`)
	}
	t.Cleanup(func() { Exec, Retries = prev, prevRetries })

	out, verdict, detail := ProbeDetail("get", "statefulset", "loki-ingester")
	if out != nil {
		t.Errorf("a failed probe returned bytes: %q", out)
	}
	if verdict != Unknown {
		t.Errorf("an RBAC denial classified as %v, want Unknown", verdict)
	}
	if !strings.Contains(detail, "cannot patch resource") {
		t.Fatalf("ProbeDetail dropped the apiserver's own words, which are the ONLY thing that tells "+
			"an RBAC fault from a dead cluster from a typo'd kubeconfig. detail = %q", detail)
	}
}

func TestProbeDetailIsUnknownRatherThanFoundWhenTheLoopNeverRuns(t *testing.T) {
	// Verdict's zero value is Found, so a Retries set to zero would return a FOUND on
	// nil bytes — which ExistsOK reports as exists=true. Retries is an exported
	// package var and callers do lower it.
	prev := Retries
	Retries = 0
	t.Cleanup(func() { Retries = prev })
	if _, verdict, _ := ProbeDetail("get", "pods"); verdict == Found {
		t.Error("a probe that never ran reported FOUND on nil bytes, which ExistsOK turns into " +
			"exists=true — a green on a question nobody asked")
	}
}
