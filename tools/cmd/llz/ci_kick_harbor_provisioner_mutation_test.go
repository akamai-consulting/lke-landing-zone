package main

import (
	"errors"
	"strings"
	"testing"
)

// withKickJobPollKnobs shrinks the Job poll budget/interval for a test.
func withKickJobPollKnobs(t *testing.T, timeout, interval int) {
	t.Helper()
	prevT, prevI := kickHarborJobTimeout, kickHarborJobInterval
	kickHarborJobTimeout, kickHarborJobInterval = timeout, interval
	t.Cleanup(func() { kickHarborJobTimeout, kickHarborJobInterval = prevT, prevI })
}

// kickHappyHandler answers every step of the kick flow as a success.
func kickHappyHandler(a string) ([]byte, error) {
	switch {
	case strings.HasPrefix(a, "-n harbor get cronjob"),
		strings.HasPrefix(a, "-n harbor wait deploy/harbor-core"),
		strings.HasPrefix(a, "-n harbor create job"),
		strings.HasPrefix(a, "annotate externalsecret"):
		return nil, nil
	case strings.HasPrefix(a, "-n harbor get job"):
		return []byte("1/"), nil
	}
	return nil, errors.New("unexpected kubectl args " + a)
}

// TestKickHarborCoreTimeoutZeroSkipsTheWait pins --core-timeout=0 as "skip the
// wait", per its own flag help. A `kubectl wait --timeout=0s` is not a skip — it
// is an immediate failure that emits a spurious "harbor-core not Available"
// warning on every standby/minimal instance that deliberately turns the wait off.
func TestKickHarborCoreTimeoutZeroSkipsTheWait(t *testing.T) {
	withKickJobPollKnobs(t, 1, 0)
	calls := kickExecLog(t, kickHappyHandler)
	captureStdoutStderr(t, func() {
		runKickHarborProvisioner("harbor", "harbor-robot-provisioner", 0)
	})
	for _, c := range *calls {
		if strings.Contains(c, "wait deploy/harbor-core") {
			t.Errorf("--core-timeout=0 must skip the wait entirely, but ran: %q", c)
		}
	}
}

// TestKickHarborWarnsOnlyOnRealFailures pins the warn/success branches of the two
// best-effort shell-outs. Every failure path here exits 0 on purpose, so the
// ::warning:: annotations are the only signal an operator gets — a warning
// emitted on success trains everyone to ignore the ones that matter, and a
// success line printed over a failure hides that the ExternalSecrets were never
// force-synced (the cert-automation chain then waits out the refreshInterval).
func TestKickHarborWarnsOnlyOnRealFailures(t *testing.T) {
	withKickJobPollKnobs(t, 1, 0)

	// Everything succeeds: no warnings, and the force-sync is reported done.
	kickExecLog(t, kickHappyHandler)
	out, errOut := captureStdoutStderr(t, func() {
		runKickHarborProvisioner("harbor", "harbor-robot-provisioner", 60)
	})
	if strings.Contains(errOut, "harbor-core not Available") {
		t.Errorf("a successful harbor-core wait must not warn:\nstderr:\n%s", errOut)
	}
	if strings.Contains(errOut, "force-sync of ExternalSecrets after the kick failed") {
		t.Errorf("a successful force-sync must not warn:\nstderr:\n%s", errOut)
	}
	if !strings.Contains(out, "force-synced all ExternalSecrets") {
		t.Errorf("a successful force-sync must say so:\nstdout:\n%s", out)
	}

	// harbor-core never becomes Available and the force-sync annotate fails: both
	// warn, and neither aborts the command.
	kickExecLog(t, func(a string) ([]byte, error) {
		switch {
		case strings.HasPrefix(a, "-n harbor wait deploy/harbor-core"):
			return nil, errors.New("timed out waiting for the condition")
		case strings.HasPrefix(a, "annotate externalsecret"):
			return nil, errors.New("connection refused")
		}
		return kickHappyHandler(a)
	})
	out, errOut = captureStdoutStderr(t, func() {
		runKickHarborProvisioner("harbor", "harbor-robot-provisioner", 60)
	})
	if !strings.Contains(errOut, "harbor-core not Available") {
		t.Errorf("a failed harbor-core wait must warn:\nstderr:\n%s", errOut)
	}
	if !strings.Contains(errOut, "force-sync of ExternalSecrets after the kick failed") {
		t.Errorf("a failed force-sync must warn:\nstderr:\n%s", errOut)
	}
	if strings.Contains(out, "force-synced all ExternalSecrets") {
		t.Errorf("a failed force-sync must not claim success:\nstdout:\n%s", out)
	}
}

// TestWaitKickHarborJobBudgetIsSeconds pins the units of both Job-poll knobs.
// They are seconds: a budget that collapses to ~0 gives the kicked Job a single
// poll (it is then always reported as "did not finish", losing the very tick this
// command exists to observe), and an interval that collapses to ~0 turns the
// wait into a kubectl hot loop against the apiserver for the whole budget.
func TestWaitKickHarborJobBudgetIsSeconds(t *testing.T) {
	withKickJobPollKnobs(t, 1, 1) // 1s budget, 1s between polls
	polls := 0
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "get job") {
			polls++
		}
		return []byte("/"), nil // never terminal
	})

	succeeded, failed := waitKickHarborJob("harbor")
	if succeeded || failed {
		t.Errorf("a Job that never reports terminal = (%v,%v), want (false,false)", succeeded, failed)
	}
	if polls != 2 {
		t.Errorf("polled %d time(s) in a 1s budget at a 1s interval, want 2", polls)
	}
}
