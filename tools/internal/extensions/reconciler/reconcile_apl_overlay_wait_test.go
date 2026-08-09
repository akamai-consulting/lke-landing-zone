package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghgitdata"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/reconcilelanes"
)

// fakeObjCredStore serves readObjPlatformCreds. seeded flips the KV read from
// "not there yet" to the bootstrap credential, which is the transition the whole
// waiter exists to notice.
type fakeObjCredStore struct {
	seeded *atomic.Bool
	err    error
}

func (f fakeObjCredStore) Get(_ context.Context, path, key string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	if path != objPlatformPath || key != objCredAccessField {
		return "", false, nil
	}
	if !f.seeded.Load() {
		return "", false, nil // mint-bootstrap-objkeys has not run yet
	}
	return "AKIAEXAMPLE", true, nil
}

// withObjCredSeams points readObjPlatformCreds at fakeObjCredStore and returns the
// flag that seeds it.
func withObjCredSeams(t *testing.T, storeErr error) *atomic.Bool {
	t.Helper()
	seeded := &atomic.Bool{}
	gc, ol, oj := openbaoGetClientFn, reconcilelanes.OpenBaoLoginFn, reconcilelanes.OpenBaoJWTFn
	openbaoGetClientFn = func(string, string) (interface {
		Get(ctx context.Context, path, key string) (string, bool, error)
	}, error) {
		return fakeObjCredStore{seeded: seeded, err: storeErr}, nil
	}
	reconcilelanes.OpenBaoLoginFn = func(context.Context, string, string) (string, error) { return "tok", nil }
	reconcilelanes.OpenBaoJWTFn = func() (string, error) { return "jwt", nil }
	t.Cleanup(func() { openbaoGetClientFn, reconcilelanes.OpenBaoLoginFn, reconcilelanes.OpenBaoJWTFn = gc, ol, oj })
	return seeded
}

// withFastPreconditionPoll shrinks the bootstrap-window poll so the tests do not
// wait out the production 15s. The give-up window is left generous so it cannot
// race the poll; TestWaitForAplOverlayPreconditionGivesUp sets it deliberately.
func withFastPreconditionPoll(t *testing.T) {
	t.Helper()
	poll, win := aplOverlayPreconditionPoll, aplOverlayPreconditionWindow
	aplOverlayPreconditionPoll = time.Millisecond
	aplOverlayPreconditionWindow = time.Minute
	t.Cleanup(func() { aplOverlayPreconditionPoll, aplOverlayPreconditionWindow = poll, win })
}

// The kick fires when the credential ARRIVES — the whole point. Before this, the
// lane's only trigger after a seed was its 300s resync floor, which put a full
// interval of dead wait in front of Loki's move onto S3 (the measured converge
// tail on 7 consecutive release-e2e runs).
func TestWaitForAplOverlayPreconditionKicksOnCredentialArrival(t *testing.T) {
	setAplOverlayEnv(t)
	withFastPreconditionPoll(t)
	seeded := withObjCredSeams(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	kicks := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		waitForAplOverlayPreconditionThenKick(ctx, func() { kicks <- struct{}{} })
	}()

	// Nothing to do yet: the credential is unseeded, so a kick here would land on a
	// pass that can only skip obj.yaml.
	select {
	case <-kicks:
		t.Fatal("kicked before the obj credential was seeded — the pass would have no-opped")
	case <-time.After(20 * time.Millisecond):
	}

	seeded.Store(true) // `llz ci mint-bootstrap-objkeys`

	select {
	case <-kicks:
	case <-ctx.Done():
		t.Fatal("the credential was seeded and no kick followed — the lane is back to waiting out its resync floor")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the waiter did not return after kicking")
	}
	// Exactly one: the waiter's job is the transition, not a second cadence.
	if len(kicks) != 0 {
		t.Errorf("waiter kicked %d extra time(s) after the transition", len(kicks))
	}
}

// On a live cluster (and every pod restart on one) the credential is already there.
// The waiter must cost that case nothing and must NOT kick — the manager's own
// initial pass already covers it.
func TestWaitForAplOverlayPreconditionAlreadyMetReturnsWithoutKicking(t *testing.T) {
	setAplOverlayEnv(t)
	withFastPreconditionPoll(t)
	seeded := withObjCredSeams(t, nil)
	seeded.Store(true)

	kicked := false
	waitForAplOverlayPreconditionThenKick(context.Background(), func() { kicked = true })
	if kicked {
		t.Error("kicked on an already-configreadiness.Satisfied precondition — a redundant pass on every pod start")
	}
}

// The precondition is a scheduling hint, not a health check: OpenBao is genuinely
// unreachable early (the reconciler starts at wave 0, before it), and that must read
// as "not yet", never as configreadiness.Satisfied.
func TestAplOverlayPreconditionUnmetCases(t *testing.T) {
	t.Run("openbao unreachable", func(t *testing.T) {
		setAplOverlayEnv(t)
		withObjCredSeams(t, errors.New("connection refused"))
		if aplOverlayPreconditionMet(context.Background()) {
			t.Error("an unreachable OpenBao reported the precondition met")
		}
	})
	t.Run("repo token not synced", func(t *testing.T) {
		setAplOverlayEnv(t)
		t.Setenv("APL_VALUES_REPO_TOKEN", "") // env empty → falls through to the mounted file
		orig := ghgitdata.AplValuesTokenFile
		ghgitdata.AplValuesTokenFile = "/nonexistent/llz-apl-values-token" // mounted file absent too
		t.Cleanup(func() { ghgitdata.AplValuesTokenFile = orig })
		seeded := withObjCredSeams(t, nil)
		seeded.Store(true)
		if aplOverlayPreconditionMet(context.Background()) {
			t.Error("met without the ESO-synced repo token — the pass cannot push")
		}
	})
	t.Run("misconfigured env", func(t *testing.T) {
		t.Setenv("GH_REPO", "")
		t.Setenv("REGION", "")
		seeded := withObjCredSeams(t, nil)
		seeded.Store(true)
		if aplOverlayPreconditionMet(context.Background()) {
			t.Error("met on an env the lane cannot even build a config from")
		}
	})
}

// The credential is NOT guaranteed to arrive: an instance that declares no
// objectStorage.cluster never seeds secret/obj/platform, which is a supported
// configuration and not a fault. Unbounded, the waiter would then poll OpenBao
// — a k8s-auth login plus a KV read — every 15s for the life of that pod, forever.
// It must give up and leave the lane on its resync floor.
func TestWaitForAplOverlayPreconditionGivesUpWhenTheCredentialNeverArrives(t *testing.T) {
	setAplOverlayEnv(t)
	withFastPreconditionPoll(t)
	withObjCredSeams(t, nil) // never seeded — the no-object-storage instance

	prev := aplOverlayPreconditionWindow
	aplOverlayPreconditionWindow = 30 * time.Millisecond
	t.Cleanup(func() { aplOverlayPreconditionWindow = prev })

	kicked := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A live context: only the window may end this, never a cancellation.
		waitForAplOverlayPreconditionThenKick(context.Background(), func() { kicked = true })
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never gave up — it would poll OpenBao for the life of the pod on any instance without object storage")
	}
	if kicked {
		t.Error("kicked without the credential — the pass can only skip obj.yaml")
	}
}

// Giving up must not release the watch: the manager reads a returning watch as a
// dropped stream, so a lane that gave up would then re-establish (and re-poll) every
// watchReconnectBackoff — turning the bound into a worse leak than the one it fixes.
func TestWatchAplOverlayPreconditionHoldsAfterGivingUp(t *testing.T) {
	setAplOverlayEnv(t)
	withFastPreconditionPoll(t)
	withObjCredSeams(t, nil) // never seeded → the waiter gives up

	prev := aplOverlayPreconditionWindow
	aplOverlayPreconditionWindow = 20 * time.Millisecond
	t.Cleanup(func() { aplOverlayPreconditionWindow = prev })

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- watchAplOverlayPrecondition(ctx, func() {}) }()

	select {
	case <-errc:
		t.Fatal("watch returned after giving up — the manager would re-establish it in a loop")
	case <-time.After(200 * time.Millisecond): // well past the window
	}
	cancel()
	select {
	case <-errc:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return after its context was cancelled")
	}
}

// The waiter only matters if the lane is actually WIRED to it. Without a watch the
// manager runs apl-overlay on runReconcilerLoop — initial pass plus interval ticks —
// which is exactly the trigger set that made a seeded credential wait out the full
// resync floor.
func TestBuildReconcilersAplOverlayCarriesPreconditionWatch(t *testing.T) {
	reg := metrics.NewRegistry()
	client := srvClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(nodeList())
	})
	identity := func(r func(context.Context) error) func(context.Context) error { return r }

	recs := buildReconcilers(reg, client, reconcileOpts{
		reconcileAplOverlay: true, aplOverlayInterval: 5 * time.Minute,
	}, identity)

	var lane *reconciler
	for i := range recs {
		if recs[i].name == "apl-overlay" {
			lane = &recs[i]
		}
	}
	if lane == nil {
		t.Fatalf("apl-overlay lane not built: %v", names(recs))
	}
	if lane.watch == nil {
		t.Error("apl-overlay carries no watch — a credential seeded mid-bootstrap waits out the whole resync floor")
	}
	// The interval stays the day-2 cadence; the watch is the bootstrap fast path,
	// not a replacement for the floor (which still backstops ErrRefNotFound).
	if lane.interval != 5*time.Minute {
		t.Errorf("apl-overlay interval = %v, want the configured 5m resync floor", lane.interval)
	}
}

// The watch HOLDS after kicking. The manager reads a returning watch as a dropped
// stream and re-establishes it after watchReconnectBackoff, so returning once
// configreadiness.Satisfied would hot-loop the lane at the reconnect cadence for the pod's life.
func TestWatchAplOverlayPreconditionHoldsUntilContextDone(t *testing.T) {
	setAplOverlayEnv(t)
	withFastPreconditionPoll(t)
	seeded := withObjCredSeams(t, nil)
	seeded.Store(true) // configreadiness.Satisfied up front → the waiter returns at once

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- watchAplOverlayPrecondition(ctx, func() {}) }()

	select {
	case <-errc:
		t.Fatal("watch returned while its context was live — the manager would treat that as a dropped stream and re-establish it in a loop")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("watch returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not return after its context was cancelled")
	}
}
