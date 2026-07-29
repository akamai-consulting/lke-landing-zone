package main

import (
	"errors"
	"testing"
	"time"
)

// waitForBaoState spends its budget by ACCUMULATING the interval, not by reading a
// clock:
//
//	for elapsed := time.Duration(0); ; elapsed += interval { … if elapsed+interval > budget { return false } … }
//
// so the interval is the only thing that makes the budget finite. A zero interval
// leaves `elapsed` pinned at 0 forever and the wait never returns — and nothing
// asserted the interval, because withBaoSleep counts waits and throws the duration
// away. waitForAutoUnseal is the caller that passes it (5s for the leader, 5s for
// each follower), so both call sites are pinned below.

// recordBaoSleep swaps the bao poll seam for one that records the interval it was
// asked to wait, instead of only counting the waits like withBaoSleep does.
func recordBaoSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	orig := baoSleep
	slept := new([]time.Duration)
	baoSleep = func(d time.Duration) { *slept = append(*slept, d) }
	t.Cleanup(func() { baoSleep = orig })
	return slept
}

// baoStatusScript answers `bao status -format=json` per pod: sealed for the first
// unsealAt-1 reads, unsealed from then on.
//
// The cap is the same safeguard pollRecorder's clock jump provides: waitForBaoState
// only terminates if its interval is non-zero, so a stub that answered "sealed"
// forever would hang the suite under exactly the mutation these tests exist to
// catch. Answering "unsealed" past the cap turns that into a failed assertion.
func baoStatusScript(t *testing.T, reads map[string]int, unsealAt, maxReads int) {
	t.Helper()
	const (
		sealed   = `{"initialized":true,"sealed":true}`
		unsealed = `{"initialized":true,"sealed":false}`
	)
	withBaoExec(t, func(pod, _, _ string, _ ...string) (string, string, error) {
		reads[pod]++
		if reads[pod] >= unsealAt || reads[pod] > maxReads {
			return unsealed, "", nil
		}
		return sealed, "", nil
	})
	// dumpBaoDiagnostics shells out for the stuck pod's container log; keep it
	// hermetic rather than spawning a real kubectl.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("no cluster") })
}

// Every pod converges on its 3rd status read, so the loops terminate on read count
// whatever the interval is; what is asserted is the two waits each pod takes.
func TestWaitForAutoUnsealPollsEveryFiveSeconds(t *testing.T) {
	reads := map[string]int{}
	baoStatusScript(t, reads, 3, 50)
	slept := recordBaoSleep(t)

	var err error
	captureStdout(t, func() { err = waitForAutoUnseal(2*time.Minute, 2*time.Minute) })
	if err != nil {
		t.Fatalf("every pod auto-unsealed on its 3rd read but the wait failed: %v", err)
	}
	for _, pod := range openbaoPodNames {
		if reads[pod] != 3 {
			t.Fatalf("%s read %d times, want 3", pod, reads[pod])
		}
	}
	// Two waits per pod (after reads 1 and 2), across the leader and both followers.
	wantEverySleepAt(t, *slept, 5*time.Second, 2*len(openbaoPodNames))
}

// The budget really is spent at that interval: a pod that never unseals must give
// up after budget/interval waits rather than polling forever, and the leader's
// failure must abort before any follower is touched.
func TestWaitForAutoUnsealLeaderTimeoutIsBudgetOverInterval(t *testing.T) {
	reads := map[string]int{}
	baoStatusScript(t, reads, 1<<30, 50) // never unseals, until the anti-hang cap
	slept := recordBaoSleep(t)

	var err error
	captureStdout(t, func() { err = waitForAutoUnseal(20*time.Second, time.Minute) })
	if err == nil {
		t.Fatal("a leader that never auto-unseals must fail the wait, not pass it")
	}
	if want := "leader " + openbaoPodNames[0] + " not initialized+unsealed within 20s"; err.Error() != want {
		t.Fatalf("err = %q, want %q — this verdict is what sends the operator to the unseal-key Secret instead of to raft", err, want)
	}
	for _, pod := range openbaoPodNames[1:] {
		if reads[pod] != 0 {
			t.Fatalf("%s was polled %d times; a leader that never unsealed must abort before the followers — they can only join a serving leader", pod, reads[pod])
		}
	}
	// A 20s budget at a 5s cadence: status reads at elapsed 0, 5, 10, 15 and 20,
	// waiting after each of the first four; from elapsed 20 the next wait would
	// overrun the budget, so the wait gives up. dumpBaoDiagnostics adds one read.
	if reads[openbaoPodNames[0]] != 6 {
		t.Fatalf("leader read %d times, want 6 (5 polls over a 20s budget at 5s, plus the timeout diagnostic)", reads[openbaoPodNames[0]])
	}
	wantEverySleepAt(t, *slept, 5*time.Second, 4)
}
