package teardown

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// captureStdoutStderr runs fn with both streams redirected and returns what each
// received. The teardown verbs are warn-don't-fail by design, so their printed
// verdict is the only observable of which branch ran.
func captureStdoutStderr(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wo, we
	fn()
	wo.Close()
	we.Close()
	o, _ := io.ReadAll(ro)
	e, _ := io.ReadAll(re)
	return string(o), string(e)
}

// TestTeardownForceDeleteReportsRealOutcomes pins which branch each delete result
// takes. These verbs delete real infrastructure and only ever WARN, so their
// output is the entire audit trail: a delete reported as issued when it failed
// (or warned about when it succeeded) sends the operator after the wrong thing,
// and an "already deleted" line printed while the cluster is still live is the
// worst of the two — it says the account is clean when it is not.
func TestTeardownForceDeleteReportsRealOutcomes(t *testing.T) {
	fake := &fakeTeardownClient{
		clusters:  []uint64{777},
		firewalls: []map[string]any{{"id": float64(42), "label": "e2e-lke-nodes"}},
	}
	dir, _ := withTeardown(t, fake, teardownTFVars)
	d := stubTerraformOutputs(t, map[string]string{})

	out, errOut := captureStdoutStderr(t, func() {
		if err := RunForceDelete(d, "e2e", dir); err != nil {
			t.Errorf("force-delete: %v", err)
		}
	})
	if !strings.Contains(out, "DELETE cluster 777 issued") {
		t.Errorf("a successful cluster DELETE must be reported as issued:\nstdout:\n%s", out)
	}
	if !strings.Contains(out, "DELETE firewall 42 (deletion is asynchronous)") {
		t.Errorf("a successful firewall DELETE must be reported as issued:\nstdout:\n%s", out)
	}
	if strings.Contains(out, "not found — already deleted") {
		t.Errorf("a cluster that IS present must not be reported as already deleted:\nstdout:\n%s", out)
	}
	if strings.Contains(errOut, "::warning::") {
		t.Errorf("nothing failed, so nothing may warn:\nstderr:\n%s", errOut)
	}

	// The inverse: a failing DELETE warns instead of claiming success.
	fake.clusters = []uint64{777}
	fake.deleteErr = map[string]error{"/v4/networking/firewalls/42": errors.New("boom")}
	out, errOut = captureStdoutStderr(t, func() {
		if err := RunForceDelete(d, "e2e", dir); err != nil {
			t.Errorf("force-delete with a failing delete must warn, not error: %v", err)
		}
	})
	if !strings.Contains(errOut, "DELETE firewall 42 failed") {
		t.Errorf("a failed firewall DELETE must warn:\nstderr:\n%s", errOut)
	}
	if strings.Contains(out, "DELETE firewall 42 (deletion is asynchronous)") {
		t.Errorf("a failed DELETE must not be reported as issued:\nstdout:\n%s", out)
	}

	// No cluster on the account → the explicit "already deleted" line.
	fake.clusters = nil
	out, _ = captureStdoutStderr(t, func() {
		if err := RunForceDelete(d, "e2e", dir); err != nil {
			t.Errorf("force-delete: %v", err)
		}
	})
	if !strings.Contains(out, "not found — already deleted") {
		t.Errorf("an absent cluster must be reported as already deleted:\nstdout:\n%s", out)
	}
}

// TestForceDeleteClusterSleepsBetweenAttemptsOnly pins the back-off placement of
// the delete-verify retry: forceDeleteClusterDelay is 15s in production, so a
// sleep after the FINAL attempt adds a quarter minute of dead time to every
// always()-path teardown, and sleeping only on the last attempt removes the
// settle window the retry exists to give the async deletion.
func TestForceDeleteClusterSleepsBetweenAttemptsOnly(t *testing.T) {
	fake := &fakeTeardownClient{
		clusters:  []uint64{777},
		deleteErr: map[string]error{"/v4beta/lke/clusters/777": errors.New("cluster stuck deleting")},
	}
	dir, _ := withTeardown(t, fake, teardownTFVars)
	d := stubTerraformOutputs(t, map[string]string{})

	prevA := forceDeleteClusterAttempts
	forceDeleteClusterAttempts = 3
	t.Cleanup(func() { forceDeleteClusterAttempts = prevA })

	sleeps := 0
	teardownSleep = func(time.Duration) { sleeps++ } // withTeardown restores the original

	captureStdoutStderr(t, func() {
		if err := RunForceDelete(d, "e2e", dir); err != nil {
			t.Errorf("wedged cluster must warn, not error: %v", err)
		}
	})
	if sleeps != 2 {
		t.Errorf("back-off slept %d time(s) across 3 attempts, want 2 (between attempts only)", sleeps)
	}
}

// TestDeleteVPCRetryReportsEveryInterveningFailure pins the retry-message
// placement of the VPC delete loop: one line per attempt that will be retried,
// none after the last (the final verdict is the ::warning:: line, and a bogus
// "retrying in" after it tells the operator to wait for a retry that never comes).
func TestDeleteVPCRetryReportsEveryInterveningFailure(t *testing.T) {
	fake := &fakeTeardownClient{
		vpcs:      []map[string]any{{"id": float64(55), "label": "e2e-lke-vpc"}},
		deleteErr: map[string]error{"/v4/vpcs/55": errors.New("409 in use")},
	}
	dir, _ := withTeardown(t, fake, teardownTFVars)
	d := stubTerraformOutputs(t, map[string]string{})

	out, errOut := captureStdoutStderr(t, func() {
		if err := RunDeleteVPC(d, "e2e", dir, "", 3, 0, false); err != nil {
			t.Errorf("exhausted retries should warn, not error: %v", err)
		}
	})
	if n := strings.Count(out, "delete failed (attempt"); n != 2 {
		t.Errorf("printed %d retry line(s) across 3 attempts, want 2:\nstdout:\n%s", n, out)
	}
	if !strings.Contains(errOut, "still not deletable after 3 attempts") {
		t.Errorf("the final verdict warning is missing:\nstderr:\n%s", errOut)
	}
}

// TestDeleteVPCRetryDelayIsSeconds pins the unit of --retry-delay. The whole
// point of the wait is to outlast the 409/in-use window while the cluster and
// its NodeBalancers release the subnet; a delay that collapses to ~0 burns the
// whole attempt budget in microseconds and leaks the VPC.
func TestDeleteVPCRetryDelayIsSeconds(t *testing.T) {
	fake := &fakeTeardownClient{
		vpcs:      []map[string]any{{"id": float64(55), "label": "e2e-lke-vpc"}},
		deleteErr: map[string]error{"/v4/vpcs/55": errors.New("409 in use")},
	}
	dir, _ := withTeardown(t, fake, teardownTFVars)
	d := stubTerraformOutputs(t, map[string]string{})

	// 2 attempts ⇒ exactly one 1-second back-off. time.Sleep is called directly
	// here (no seam), so the elapsed time is the only observable.
	start := time.Now()
	captureStdoutStderr(t, func() {
		if err := RunDeleteVPC(d, "e2e", dir, "", 2, 1, false); err != nil {
			t.Errorf("delete-vpc: %v", err)
		}
	})
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("retry back-off took %v, want ≥1s (--retry-delay is in seconds)", elapsed)
	}
}
