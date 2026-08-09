package reconciler

import (
	"context"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghgitdata"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

// These cover the cmd/llz WRAPPER: the env contract and the ESO-synced-token gate.
// The sync orchestration is tested in internal/apl/overlay against a fake Repo.

// setAplOverlayEnv sets the minimal env contract and restores it via t.Setenv.
func setAplOverlayEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GH_REPO", "acme/instance")
	t.Setenv("APL_VALUES_REPO_TOKEN", "tok")
	t.Setenv("REGION", "primary")
	t.Setenv("APL_VALUES_SOURCE_BRANCH", "main")
	// leave APL_VALUES_BRANCH unset → defaults to apl-primary
	t.Setenv("APL_VALUES_BRANCH", "")
}

// Missing render-time-static env (GH_REPO/REGION) → a loud misconfiguration error.
func TestReconcileAplOverlay_MisconfigErrors(t *testing.T) {
	t.Setenv("GH_REPO", "")
	t.Setenv("REGION", "")
	t.Setenv("APL_VALUES_REPO_TOKEN", "tok") // token present — the error is about repo/region
	if err := reconcileAplOverlay(context.Background(), metrics.NewRegistry()); err == nil {
		t.Error("missing GH_REPO/REGION must error")
	}
}

// The ESO-synced apl-values-repo-token not being present yet is a transient NO-OP
// (the pod starts at wave 0, before the OpenBao store serves), not a misconfig.
func TestReconcileAplOverlay_MissingTokenIsNoOp(t *testing.T) {
	setAplOverlayEnv(t)
	t.Setenv("APL_VALUES_REPO_TOKEN", "") // env empty → falls through to the mounted file
	orig := ghgitdata.AplValuesTokenFile
	ghgitdata.AplValuesTokenFile = "/nonexistent/llz-apl-values-token" // mounted file absent too
	t.Cleanup(func() { ghgitdata.AplValuesTokenFile = orig })
	if err := reconcileAplOverlay(context.Background(), metrics.NewRegistry()); err != nil {
		t.Errorf("unsynced token must be a no-op, got: %v", err)
	}
}
