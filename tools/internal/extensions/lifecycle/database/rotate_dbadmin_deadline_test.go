package database

import (
	"context"
	"strings"
	"testing"
	"time"
)

// withAdvancingDBClock installs a fake clock that moves ONLY when waitDBActive
// waits — the production shape, where the budget is spent by sleeping. The
// existing harness stubs dbAdminSleep as a no-op against a frozen dbAdminNow, so
// its clock never reaches the deadline at all: no test had ever driven this loop
// to expiry.
func withAdvancingDBClock(t *testing.T) {
	t.Helper()
	origNow, origSleep := dbAdminNow, dbAdminSleep
	t.Cleanup(func() { dbAdminNow, dbAdminSleep = origNow, origSleep })
	now := time.Unix(1_800_000_000, 0)
	dbAdminNow = func() time.Time { return now }
	dbAdminSleep = func(d time.Duration) { now = now.Add(d) }
}

// A cluster that never returns to `active` must fail at the deadline, and the
// verdict has to carry both the budget and the LAST status seen — this error is
// raised AFTER the reset has landed, so it is the operator's only description of
// what the cluster was doing while the new credential went unpersisted.
func TestWaitDBActiveDeadline(t *testing.T) {
	withAdvancingDBClock(t)

	api := &fakeDBAPI{statuses: []string{"updating"}} // last value repeats forever
	tgt := dbAdminTarget{name: "shared", id: 12345, path: dbAdminSeedRoot + "shared"}

	err := waitDBActive(context.Background(), api, tgt)
	if err == nil {
		t.Fatal("a cluster stuck in `updating` must fail the wait — returning nil hands a half-applied reset to the caller as success")
	}
	if !strings.Contains(err.Error(), "did not return to `active` within "+dbAdminActiveTimeout.String()) {
		t.Fatalf("err = %v, want the deadline verdict naming the budget", err)
	}
	if !strings.Contains(err.Error(), `(last status "updating")`) {
		t.Fatalf("err = %v, want the last observed status — without it the operator cannot tell a stuck reset from a cluster in maintenance", err)
	}
	// The budget was spent at the poll cadence, not abandoned early: probes at
	// elapsed 0, 10s, … up to and including the one landing on the deadline.
	// (Deriving the count from the two constants is also what keeps a collapsed
	// dbAdminPollInterval loud: at zero it is a compile-time division by zero, so
	// the build fails instead of this loop spinning against a clock that the wait
	// can no longer move.)
	if want := int(dbAdminActiveTimeout/dbAdminPollInterval) + 1; api.instanceGets != want {
		t.Fatalf("polled %d times, want %d (a %s budget at %s)", api.instanceGets, want, dbAdminActiveTimeout, dbAdminPollInterval)
	}
}

// ...and the `active` exit really is an EARLY exit: an already-active cluster
// returns on the first poll rather than riding the budget out.
func TestWaitDBActiveReturnsOnTheFirstActiveRead(t *testing.T) {
	withAdvancingDBClock(t)

	api := &fakeDBAPI{statuses: []string{"active"}}
	tgt := dbAdminTarget{name: "shared", id: 12345, path: dbAdminSeedRoot + "shared"}

	if err := waitDBActive(context.Background(), api, tgt); err != nil {
		t.Fatalf("an active cluster must satisfy the wait immediately: %v", err)
	}
	if api.instanceGets != 1 {
		t.Fatalf("polled %d times for an already-active cluster, want 1", api.instanceGets)
	}
}

// A cluster that settles part-way through must be noticed on the poll that sees
// it, not on the next one — the reset window is the interval where the stored
// credential is dead, so every extra poll is downtime.
func TestWaitDBActiveStopsOnTheSettlingPoll(t *testing.T) {
	withAdvancingDBClock(t)

	api := &fakeDBAPI{statuses: []string{"updating", "updating", "active", "resizing"}}
	tgt := dbAdminTarget{name: "shared", id: 12345, path: dbAdminSeedRoot + "shared"}

	if err := waitDBActive(context.Background(), api, tgt); err != nil {
		t.Fatalf("a cluster that settles inside the budget must satisfy the wait: %v", err)
	}
	if api.instanceGets != 3 {
		t.Fatalf("polled %d times, want 3 — the wait must return on the read that first sees `active`", api.instanceGets)
	}
}
