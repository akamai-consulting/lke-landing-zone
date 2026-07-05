package main

import (
	"context"
	"errors"
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
