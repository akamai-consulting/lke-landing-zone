package main

import (
	"errors"
	"strings"
	"testing"
)

// Every kubectl call here is best-effort, which makes the LOG the only evidence
// anything went wrong — the command's exit code deliberately hides it. The
// existing best-effort test asserted the loop kept going but never that the
// failures were reported, so each `if err != nil` guard could be inverted (or
// dropped) without a single test noticing.
func TestRunCINudgeArgoReportsEveryBestEffortFailure(t *testing.T) {
	fixedNow(t, 1700000000)
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("the server could not find the requested resource")
	})
	var err error
	errOut := captureStderr(t, func() {
		err = runCINudgeArgo(globalOpts{}, nudgeOpts{apps: []string{"platform-bootstrap"}, store: "openbao", storeTimeout: 1})
	})
	if err != nil {
		t.Fatalf("best-effort nudge must not return an error, got %v", err)
	}
	for _, want := range []string{
		"nudge platform-bootstrap: refresh annotate failed (ignored)",
		"nudge platform-bootstrap: sync patch failed (ignored)",
		"nudge: clustersecretstore/openbao revalidation bump failed (ignored)",
		"nudge: clustersecretstore/openbao not Ready within 1s",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

// ...and reports NOTHING when every call succeeds — a warning stream that cries
// wolf on the happy path is worth as little as one that stays silent on failure.
func TestRunCINudgeArgoQuietWhenEveryCallSucceeds(t *testing.T) {
	fixedNow(t, 1700000000)
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, nil })
	var err error
	var out string
	errOut := captureStderr(t, func() {
		out = captureStdout(t, func() {
			err = runCINudgeArgo(globalOpts{}, nudgeOpts{apps: []string{"platform-bootstrap"}, store: "openbao", storeTimeout: 1})
		})
	})
	if err != nil {
		t.Fatalf("nudge: %v", err)
	}
	for _, unwanted := range []string{"(ignored)", "not Ready within"} {
		if strings.Contains(errOut, unwanted) {
			t.Errorf("nothing failed, yet stderr says %q:\n%s", unwanted, errOut)
		}
	}
	if !strings.Contains(out, "clustersecretstore/openbao Ready") {
		t.Errorf("a successful store-wait must report Ready:\n%s", out)
	}
}
