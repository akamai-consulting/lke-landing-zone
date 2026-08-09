package reconcilelanes

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// fixedNow pins the clock seam. A local copy of package main's helper: nowUnix is
// this package's own seam now, and the es-store-recovery lane writes a
// revalidation annotation whose value would otherwise change every run.
func fixedNow(t *testing.T, v int64) {
	t.Helper()
	orig := nowUnix
	nowUnix = func() int64 { return v }
	t.Cleanup(func() { nowUnix = orig })
}

// ── the fence each lane acts behind ──────────────────────────────────────────

// EVERY LANE'S BINDING MUST RESOLVE, because Fenced panics otherwise and the
// daemon builds its lanes at startup — a missing name would crash the reconciler
// on boot rather than degrade it.
func TestEveryLaneNameResolvesToItsBinding(t *testing.T) {
	for _, b := range Extension().Bindings {
		if b.Kind != extension.Invariant {
			continue
		}
		got := laneBinding(b.Name)
		if got.Name != b.Name {
			t.Errorf("laneBinding(%q) returned %q", b.Name, got.Name)
		}
	}
}

// A NAME NOBODY DECLARED PANICS, and that is deliberate — the same choice
// capability.RepoForGate makes. A refusing handle would express a wiring bug as
// the cluster being unreachable, which reads as an outage and sends someone to the
// wrong place entirely.
func TestAnUndeclaredLaneNamePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("laneBinding accepted a name no binding declares — the lane would run with " +
				"a handle built from nothing")
		}
		if !strings.Contains(fmt.Sprint(r), "wiring bug") {
			t.Errorf("panic %v does not say what kind of mistake this is", r)
		}
	}()
	_ = laneBinding("no-such-lane")
}

// THE HANDLE A LANE GETS IS THE ONE ITS DECLARATION EARNED, not the daemon's full
// client. This is the property the whole fence exists for: sc-demote declares
// cluster-read + cluster-write and can patch; a lane declaring less could not.
func TestFencedGivesTheLaneItsDeclaredReach(t *testing.T) {
	k := Fenced("sc-demote", &fenceProbe{})
	if err := k.MergePatch(context.Background(), "/apis/storage.k8s.io/v1/storageclasses/x", nil); err != nil {
		t.Errorf("sc-demote cannot patch through its own binding: %v — it declares cluster-write", err)
	}
}

// And a lane whose declaration lacked cluster-write would be refused at the
// transport. Asserted against a synthetic binding rather than by weakening a real
// declaration, so this cannot pass by making the model wrong.
func TestAReadOnlyLaneWouldBeRefused(t *testing.T) {
	k := capability.KubeFor(
		extension.Binding{Kind: extension.Invariant, State: extension.Operating,
			Grants: []extension.Grant{extension.ClusterRead}},
		&fenceProbe{})
	if err := k.MergePatch(context.Background(), "/x", nil); err == nil {
		t.Error("a cluster-read lane patched the cluster")
	}
}

type fenceProbe struct{}

func (fenceProbe) GetJSON(context.Context, string) (map[string]any, int, error) {
	return map[string]any{}, 200, nil
}
func (fenceProbe) CreateJSON(context.Context, string, any) (int, error) { return 201, nil }
func (fenceProbe) MergePatch(context.Context, string, any) error        { return nil }
