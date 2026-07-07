package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeLeaseStore is an in-memory coordination Lease backing the elector tests.
type fakeLeaseStore struct {
	mu           sync.Mutex
	obj          map[string]any
	exists       bool
	getErr       error
	createStatus int // if non-zero, CreateJSON returns this (e.g. 409) and stores nothing
	creates      int
	patches      int
}

func (f *fakeLeaseStore) GetJSON(_ context.Context, _ string) (map[string]any, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, 0, f.getErr
	}
	if !f.exists {
		return nil, 404, nil
	}
	return f.obj, 200, nil
}

func (f *fakeLeaseStore) CreateJSON(_ context.Context, _ string, o any) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.createStatus != 0 {
		return f.createStatus, nil
	}
	f.obj, _ = o.(map[string]any)
	f.exists = true
	return 201, nil
}

func (f *fakeLeaseStore) MergePatch(_ context.Context, _ string, patch any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches++
	p, _ := patch.(map[string]any)
	pspec, _ := p["spec"].(map[string]any)
	if f.obj == nil {
		f.obj = map[string]any{}
	}
	spec, _ := f.obj["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
		f.obj["spec"] = spec
	}
	for k, v := range pspec {
		spec[k] = v
	}
	return nil
}

func (f *fakeLeaseStore) holder() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	spec, _ := f.obj["spec"].(map[string]any)
	h, _ := spec["holderIdentity"].(string)
	return h
}

func leaseObj(holder string, renew time.Time) map[string]any {
	return map[string]any{"spec": map[string]any{
		"holderIdentity": holder,
		"renewTime":      renew.UTC().Format(time.RFC3339Nano),
	}}
}

func newElectorAt(f leaseClient, identity string, now time.Time) *leaderElector {
	return newLeaderElector(f, "ns", "l", identity, func() time.Time { return now })
}

func TestElectorAcquiresWhenAbsent(t *testing.T) {
	f := &fakeLeaseStore{}
	e := newElectorAt(f, "me", time.Unix(1000, 0))
	e.tryAcquire(context.Background())
	if !e.IsLeader() {
		t.Fatal("should become leader when the lease is absent")
	}
	if f.creates != 1 || f.holder() != "me" {
		t.Fatalf("expected one create with holder=me, got creates=%d holder=%q", f.creates, f.holder())
	}
}

func TestElectorRenewsWhenHeldBySelf(t *testing.T) {
	t0 := time.Unix(1000, 0)
	f := &fakeLeaseStore{exists: true, obj: leaseObj("me", t0)}
	e := newElectorAt(f, "me", t0.Add(5*time.Second))
	e.tryAcquire(context.Background())
	if !e.IsLeader() {
		t.Fatal("should stay leader when it already holds the lease")
	}
	if f.patches != 1 {
		t.Fatalf("expected a renew patch, got %d patches", f.patches)
	}
}

func TestElectorStandsBackWhenHeldByLivePeer(t *testing.T) {
	t0 := time.Unix(1000, 0)
	f := &fakeLeaseStore{exists: true, obj: leaseObj("peer", t0)}
	e := newElectorAt(f, "me", t0.Add(5*time.Second)) // well within the 30s duration
	e.tryAcquire(context.Background())
	if e.IsLeader() {
		t.Fatal("must not take a lease a live peer still holds")
	}
	if f.patches != 0 {
		t.Fatalf("must not patch a peer's live lease, got %d patches", f.patches)
	}
}

func TestElectorTakesOverReleasedLease(t *testing.T) {
	t0 := time.Unix(1000, 0)
	// A peer's release() clears holderIdentity but leaves a still-fresh renewTime.
	// Must be taken over immediately, NOT mistaken for a live peer and waited out.
	f := &fakeLeaseStore{exists: true, obj: leaseObj("", t0)}
	e := newElectorAt(f, "me", t0.Add(5*time.Second)) // well within the 30s duration
	e.tryAcquire(context.Background())
	if !e.IsLeader() {
		t.Fatal("should take over a released (empty-holder) lease immediately")
	}
	if f.holder() != "me" {
		t.Fatalf("takeover should rewrite holder to me, got %q", f.holder())
	}
}

func TestElectorTakesOverExpiredLease(t *testing.T) {
	t0 := time.Unix(1000, 0)
	f := &fakeLeaseStore{exists: true, obj: leaseObj("peer", t0)}
	e := newElectorAt(f, "me", t0.Add(31*time.Second)) // past the 30s duration
	e.tryAcquire(context.Background())
	if !e.IsLeader() {
		t.Fatal("should take over a peer's expired lease")
	}
	if f.holder() != "me" {
		t.Fatalf("takeover should rewrite holder to me, got %q", f.holder())
	}
}

func TestElectorNotLeaderOnGetError(t *testing.T) {
	f := &fakeLeaseStore{getErr: errors.New("apiserver down")}
	e := newElectorAt(f, "me", time.Unix(1000, 0))
	e.setLeader(true) // pretend we were leader
	e.tryAcquire(context.Background())
	if e.IsLeader() {
		t.Fatal("must drop leadership when the lease can't be confirmed")
	}
}

func TestElectorLosesCreateRace(t *testing.T) {
	f := &fakeLeaseStore{createStatus: 409} // a peer created it first
	e := newElectorAt(f, "me", time.Unix(1000, 0))
	e.tryAcquire(context.Background())
	if e.IsLeader() {
		t.Fatal("must not be leader after losing the create race")
	}
}

func TestElectorReleaseClearsHolder(t *testing.T) {
	t0 := time.Unix(1000, 0)
	f := &fakeLeaseStore{exists: true, obj: leaseObj("me", t0)}
	e := newElectorAt(f, "me", t0)
	e.setLeader(true)
	e.release()
	if e.IsLeader() {
		t.Fatal("release should drop leadership")
	}
	if f.holder() != "" {
		t.Fatalf("release should clear the holder, got %q", f.holder())
	}
}

func TestElectorHealthyStaleness(t *testing.T) {
	t0 := time.Unix(1000, 0)
	clk := t0
	e := newLeaderElector(&fakeLeaseStore{}, "ns", "l", "me", func() time.Time { return clk })

	if !e.Healthy() {
		t.Fatal("a just-constructed elector must be healthy (lastEval seeded to now)")
	}
	clk = t0.Add(e.staleAfter - time.Second)
	if !e.Healthy() {
		t.Fatal("must stay healthy within staleAfter of the last evaluation")
	}
	clk = t0.Add(e.staleAfter + time.Second)
	if e.Healthy() {
		t.Fatal("must report unhealthy once the loop hasn't evaluated within staleAfter")
	}
	// A completed evaluation (even this one, which acquires) restores health.
	e.tryAcquire(context.Background())
	if !e.Healthy() {
		t.Fatal("a fresh evaluation must restore health")
	}
}

// A live loop that keeps ERRORING is still making progress and must stay Healthy
// (so a legit transient/standby is never restarted); only a wedged loop trips.
func TestElectorMarksEvalEvenOnError(t *testing.T) {
	t0 := time.Unix(2000, 0)
	clk := t0
	e := newLeaderElector(&fakeLeaseStore{getErr: errors.New("boom")}, "ns", "l", "me", func() time.Time { return clk })

	clk = t0.Add(time.Hour) // make the seeded lastEval very stale
	if e.Healthy() {
		t.Fatal("precondition: should be stale before the next evaluation")
	}
	e.tryAcquire(context.Background()) // GET errors → logErr + setLeader(false), but still markEval
	if !e.Healthy() {
		t.Fatal("a loop that erred but returned is alive — it must be healthy, not restarted")
	}
}

func TestReconcilerHealthz(t *testing.T) {
	get := func(e *leaderElector) int {
		rec := httptest.NewRecorder()
		reconcilerHealthz(e)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rec.Code
	}
	// Election disabled (nil elector) → always ok.
	if code := get(nil); code != http.StatusOK {
		t.Fatalf("nil elector: want 200, got %d", code)
	}

	t0 := time.Unix(3000, 0)
	clk := t0
	e := newLeaderElector(&fakeLeaseStore{}, "ns", "l", "me", func() time.Time { return clk })

	if code := get(e); code != http.StatusOK {
		t.Fatalf("healthy elector: want 200, got %d", code)
	}
	clk = t0.Add(e.staleAfter + time.Second)
	if code := get(e); code != http.StatusServiceUnavailable {
		t.Fatalf("wedged elector: want 503, got %d", code)
	}
}
