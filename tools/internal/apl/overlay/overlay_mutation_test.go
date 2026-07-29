package overlay

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

// overlay_mutation_test.go pins Reconcile's post-commit branch: a genuine commit
// failure is RETURNED (the manager records up=0) and leaves the synced gauge
// unset, while a successful commit sets it. Neither is visible to the existing
// tests, which only cover the ErrRefNotFound no-op.

// objAndAppsSource is the minimal source tree that makes Reconcile reach the
// commit: an obj overlay to fill plus one app CR to toggle.
func objAndAppsSource() map[string]string {
	return map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile):          clusterspec.RenderObjOverlayShared(),
		envOverlayPath("primary", clusterspec.OverlayObjFile):  clusterspec.RenderObjOverlayEnv("primary", "us-ord-1"),
		sharedOverlayPath(clusterspec.OverlayAppsFile):         "apps:\n  knative:\n    enabled: false\n",
		envOverlayPath("primary", clusterspec.OverlayAppsFile): "",
		aplAppTarget("knative"):                                "kind: AplApp\nmetadata:\n  name: knative\nspec:\n  enabled: true\n",
	}
}

func renderedMetrics(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := reg.WriteTo(&buf); err != nil {
		t.Fatalf("render metrics: %v", err)
	}
	return buf.String()
}

// TestReconcile_CommitErrorIsReturned asserts a real API/merge failure propagates
// (only ErrRefNotFound is the expected no-op) and that the sync gauge is NOT set —
// a swallowed error would report a healthy sync that never happened.
func TestReconcile_CommitErrorIsReturned(t *testing.T) {
	boom := errors.New("github: 500 creating tree")
	repo := &fakeRepo{files: objAndAppsSource(), commitErr: boom}
	reg := metrics.NewRegistry()

	err := Reconcile(context.Background(), testCfg(), repo, credsOK, reg)
	if !errors.Is(err, boom) {
		t.Fatalf("Reconcile err = %v, want the commit error %v", err, boom)
	}
	if got := renderedMetrics(t, reg); strings.Contains(got, "llz_apl_overlay_synced") {
		t.Errorf("a failed commit must not set the synced gauge:\n%s", got)
	}
}

// TestReconcile_SuccessSetsSyncedGauge is the other side of the same branch: on a
// clean commit Reconcile falls THROUGH to the gauge, which is the reconciler's
// only positive liveness signal.
func TestReconcile_SuccessSetsSyncedGauge(t *testing.T) {
	repo := &fakeRepo{files: objAndAppsSource()}
	reg := metrics.NewRegistry()

	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, reg); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := renderedMetrics(t, reg)
	if !strings.Contains(got, "llz_apl_overlay_synced") {
		t.Fatalf("successful sync must set llz_apl_overlay_synced:\n%s", got)
	}
	if !strings.Contains(got, `branch="apl-primary"`) {
		t.Errorf("synced gauge must be labelled with the target branch:\n%s", got)
	}
}

// TestReconcile_RefNotFoundLeavesGaugeUnset keeps the ErrRefNotFound no-op honest:
// it returns nil, but nothing was synced, so the gauge must stay unset.
func TestReconcile_RefNotFoundLeavesGaugeUnset(t *testing.T) {
	repo := &fakeRepo{files: objAndAppsSource(), commitErr: ErrRefNotFound}
	reg := metrics.NewRegistry()

	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, reg); err != nil {
		t.Fatalf("missing target branch must be a no-op, got: %v", err)
	}
	if got := renderedMetrics(t, reg); strings.Contains(got, "llz_apl_overlay_synced") {
		t.Errorf("an un-bootstrapped target branch must not set the synced gauge:\n%s", got)
	}
}
