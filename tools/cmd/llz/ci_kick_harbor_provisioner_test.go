package main

import (
	"errors"
	"strings"
	"testing"
)

// kickExecLog stubs execOutput/execCombined for the kick flow and records every
// kubectl invocation (joined args) so tests can assert the call sequence.
func kickExecLog(t *testing.T, handler func(args string) ([]byte, error)) *[]string {
	t.Helper()
	var calls []string
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		a := strings.Join(args, " ")
		calls = append(calls, a)
		if name != "kubectl" {
			return nil, errors.New("unexpected command " + name)
		}
		return handler(a)
	})
	prevCombined := execCombined
	execCombined = func(name string, args ...string) string {
		calls = append(calls, strings.Join(args, " "))
		return ""
	}
	t.Cleanup(func() { execCombined = prevCombined })
	return &calls
}

func TestKickHarborProvisionerNoCronJob(t *testing.T) {
	calls := kickExecLog(t, func(a string) ([]byte, error) {
		return nil, errors.New("NotFound") // cronjob absent
	})
	runKickHarborProvisioner("harbor", "harbor-robot-provisioner", 0)
	if len(*calls) != 1 {
		t.Errorf("absent CronJob must short-circuit after the existence probe, got calls %v", *calls)
	}
}

func TestKickHarborProvisionerHappyPath(t *testing.T) {
	prevT, prevI := kickHarborJobTimeout, kickHarborJobInterval
	kickHarborJobTimeout, kickHarborJobInterval = 1, 0
	t.Cleanup(func() { kickHarborJobTimeout, kickHarborJobInterval = prevT, prevI })
	prevNow := nowUnix
	nowUnix = func() int64 { return 42 }
	t.Cleanup(func() { nowUnix = prevNow })

	calls := kickExecLog(t, func(a string) ([]byte, error) {
		switch {
		case strings.HasPrefix(a, "-n harbor get cronjob"):
			return nil, nil
		case strings.HasPrefix(a, "-n harbor wait deploy/harbor-core"):
			return nil, nil
		case strings.HasPrefix(a, "-n harbor create job"):
			return nil, nil
		case strings.HasPrefix(a, "-n harbor get job"):
			return []byte("1/"), nil // succeeded on first poll
		case strings.HasPrefix(a, "annotate externalsecret"):
			return nil, nil
		}
		return nil, errors.New("unexpected kubectl args " + a)
	})
	runKickHarborProvisioner("harbor", "harbor-robot-provisioner", 60)

	joined := strings.Join(*calls, "\n")
	for _, want := range []string{
		"wait deploy/harbor-core --for=condition=Available --timeout=60s",
		"delete job harbor-robot-provisioner-kick --ignore-not-found",
		"create job --from=cronjob/harbor-robot-provisioner harbor-robot-provisioner-kick",
		"annotate externalsecret --all-namespaces --all force-sync=42 --overwrite",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected a kubectl call containing %q, got:\n%s", want, joined)
		}
	}
}

func TestKickHarborProvisionerCreateDeniedStillForceSyncsNothing(t *testing.T) {
	// An admission-denied create must warn and stop BEFORE the job wait — and
	// must not fail (the function has no error return by design).
	calls := kickExecLog(t, func(a string) ([]byte, error) {
		switch {
		case strings.HasPrefix(a, "-n harbor get cronjob"):
			return nil, nil
		case strings.HasPrefix(a, "-n harbor create job"):
			return nil, errors.New("admission webhook denied")
		}
		return nil, errors.New("unexpected kubectl args " + a)
	})
	runKickHarborProvisioner("harbor", "harbor-robot-provisioner", 0)
	for _, c := range *calls {
		if strings.HasPrefix(c, "-n harbor get job") || strings.HasPrefix(c, "annotate externalsecret") {
			t.Errorf("denied create must short-circuit the wait/force-sync, but saw %q", c)
		}
	}
}

func TestKickHarborProvisionerJobFailureStillForceSyncs(t *testing.T) {
	prevT, prevI := kickHarborJobTimeout, kickHarborJobInterval
	kickHarborJobTimeout, kickHarborJobInterval = 1, 0
	t.Cleanup(func() { kickHarborJobTimeout, kickHarborJobInterval = prevT, prevI })

	forceSynced := false
	kickExecLog(t, func(a string) ([]byte, error) {
		switch {
		case strings.HasPrefix(a, "-n harbor get cronjob"):
			return nil, nil
		case strings.HasPrefix(a, "-n harbor create job"):
			return nil, nil
		case strings.HasPrefix(a, "-n harbor get job"):
			return []byte("/1"), nil // failed
		case strings.HasPrefix(a, "annotate externalsecret"):
			forceSynced = true
			return nil, nil
		}
		return nil, errors.New("unexpected kubectl args " + a)
	})
	runKickHarborProvisioner("harbor", "harbor-robot-provisioner", 0)
	if !forceSynced {
		t.Error("a failed kick Job must still force-sync ExternalSecrets (the tick may have partially seeded)")
	}
}
