package health

// argo_syncerr_test.go — an Application whose sync keeps FAILING is not "drift
// only; workload functional".
//
// Argo retries a failed sync in place: operationState.phase stays "Running" while
// the message becomes "one or more synchronization tasks completed
// unsuccessfully…". Because the phase was not Failed, that text was discarded —
// and an Application whose Deployment had never been created (Kyverno refusing an
// unpublished image) was graded OutOfSync/Healthy → CatDrift → "workload
// functional". Convergence then spent its whole budget on a downstream symptom.

import (
	"strings"
	"testing"
)

const kyvernoDenial = `one or more synchronization tasks completed unsuccessfully, reason: admission webhook "mutate.kyverno.svc-ignore" denied the request: 

resource Deployment/llz-reconciler/llz-reconciler was blocked due to the following policies 

verify-llz-image-signature:
  autogen-verify-llz-keyless: 'failed to verify image ghcr.io/o/llz:sha-abc: MANIFEST_UNKNOWN'. Retrying attempt #15`

func TestAFailingSyncIsNotDrift(t *testing.T) {
	a := ArgoApp{
		Name: "llz-reconciler", Sync: "OutOfSync", Health: "Healthy", Automated: true,
		Drifted: []string{"Deployment/llz-reconciler/llz-reconciler"},
		SyncErr: kyvernoDenial,
	}
	cat, msg := ClassifyArgoApp(a, false)
	if cat == CatDrift {
		t.Errorf("an Application whose sync keeps failing was called drift-only; its Deployment may never "+
			"have been created: %s", msg)
	}
	if cat != CatPending {
		t.Errorf("a retrying sync is in progress with a real error — want CatPending, got %v (%s)", cat, msg)
	}
	if !strings.Contains(msg, "FAILING") || !strings.Contains(msg, "denied the request") {
		t.Errorf("the report line must carry Argo's own reason, or the operator learns nothing: %q", msg)
	}
	// The whole Kyverno policy report must NOT land in a census line.
	if strings.Contains(msg, "\n") || len(msg) > 400 {
		t.Errorf("the message was not reduced to one bounded line: %q", msg)
	}
}

// A genuinely drifted app — synced cleanly, resources changed since — must still
// read as drift, or the change has just turned every OutOfSync app into a blocker.
func TestRealDriftIsStillDrift(t *testing.T) {
	a := ArgoApp{Name: "x", Sync: "OutOfSync", Health: "Healthy", Automated: true,
		Drifted: []string{"ConfigMap/x/y"}}
	if cat, msg := ClassifyArgoApp(a, false); cat != CatDrift {
		t.Errorf("an app with no sync error is drift, got %v (%s)", cat, msg)
	}
}

func TestSyncTasksFailingRecognisesOnlyTheFailureSentence(t *testing.T) {
	if !SyncTasksFailing(kyvernoDenial) {
		t.Error("the retrying-failure message was not recognised")
	}
	for _, benign := range []string{"", "successfully synced (all tasks run)", "waiting for healthy state"} {
		if SyncTasksFailing(benign) {
			t.Errorf("%q was read as a failing sync", benign)
		}
	}
}

func TestFirstLineBoundsAMultiLineMessage(t *testing.T) {
	got := FirstLine(kyvernoDenial)
	if strings.Contains(got, "\n") {
		t.Errorf("FirstLine returned multiple lines: %q", got)
	}
	if !strings.Contains(got, "denied the request") {
		t.Errorf("FirstLine dropped the useful sentence: %q", got)
	}
	if len(got) > firstLineMax+1 {
		t.Errorf("FirstLine returned %d chars, want <= %d", len(got), firstLineMax+1)
	}
	if FirstLine("\n\n   \nreal") != "real" {
		t.Errorf("leading blank lines were not skipped")
	}
}

// The parse half: a RETRYING sync keeps phase "Running", so its message has to be
// picked up there or the classifier never sees it. This is the field whose loss
// let an Application with no Deployment read as "workload functional".
func TestParseArgoAppCarriesARetryingSyncMessage(t *testing.T) {
	const raw = `{"metadata":{"name":"llz-reconciler"},"status":{
		"sync":{"status":"OutOfSync"},"health":{"status":"Healthy"},
		"operationState":{"phase":"Running","message":"one or more synchronization tasks completed unsuccessfully, reason: admission webhook denied the request. Retrying attempt #15"},
		"resources":[{"kind":"Deployment","namespace":"llz-reconciler","name":"llz-reconciler","status":"OutOfSync"}]}}`
	a, err := ParseArgoApp([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.SyncErr == "" {
		t.Fatal("a Running-but-failing sync message was dropped — the phase is not Failed, which is " +
			"exactly why this was invisible")
	}
	if a.OpErr != "" {
		t.Errorf("OpErr is for a FAILED/Error phase only, got %q", a.OpErr)
	}
	if cat, msg := ClassifyArgoApp(a, false); cat != CatPending {
		t.Errorf("ClassifyArgoApp = %v (%s), want CatPending for a failing-and-retrying sync", cat, msg)
	}

	// A Running sync that is merely IN FLIGHT must not be treated as failing.
	inFlight, _ := ParseArgoApp([]byte(`{"metadata":{"name":"x"},"spec":{"syncPolicy":{"automated":{}}},"status":{"sync":{"status":"OutOfSync"},"health":{"status":"Healthy"},"operationState":{"phase":"Running","message":"waiting for healthy state of apps/Deployment/x"}}}`))
	if inFlight.SyncErr != "" {
		t.Errorf("an in-flight sync was read as failing: %q", inFlight.SyncErr)
	}
	if cat, _ := ClassifyArgoApp(inFlight, false); cat != CatDrift {
		t.Errorf("an OutOfSync/Healthy app with a healthy in-flight sync is drift, got %v", cat)
	}
}

// FirstLine's remaining arms: an over-long single line is truncated with a
// marker, and a message with nothing but blanks degrades to the trimmed input
// rather than returning something that looks like content.
func TestFirstLineTruncatesAndDegrades(t *testing.T) {
	long := strings.Repeat("x", firstLineMax+50)
	got := FirstLine(long)
	// "…" is three BYTES, so the bound is on the excerpt, not on len(got).
	if !strings.HasSuffix(got, "…") || len(strings.TrimSuffix(got, "…")) != firstLineMax {
		t.Errorf("a long line should be truncated to %d chars plus a marker, got %d bytes: %q",
			firstLineMax, len(got), got)
	}
	if FirstLine("   \n\t\n  ") != "" {
		t.Errorf("an all-blank message should reduce to empty, got %q", FirstLine("   \n\t\n  "))
	}
}
