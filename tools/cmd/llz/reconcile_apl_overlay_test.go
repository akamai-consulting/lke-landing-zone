package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

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

// fakeOverlaySeams installs read/commit/creds fakes and restores them.
func fakeOverlaySeams(t *testing.T, read func(path string) string, creds func() (string, bool, error), commit func(files map[string]string, branch string) (string, bool, error)) {
	t.Helper()
	origRead, origCommit, origCreds := aplOverlayReadFileFn, aplOverlayCommitFn, aplOverlayObjCredsFn
	t.Cleanup(func() {
		aplOverlayReadFileFn, aplOverlayCommitFn, aplOverlayObjCredsFn = origRead, origCommit, origCreds
	})

	aplOverlayReadFileFn = func(_ context.Context, _ *http.Client, _, _, _, path string) (string, bool, error) {
		c := read(path)
		return c, c != "", nil
	}
	aplOverlayObjCredsFn = func(_ context.Context, _ string) (string, bool, error) { return creds() }
	aplOverlayCommitFn = func(_ context.Context, _ *http.Client, _, _, branch string, files map[string]string, _ string, _ int) (string, bool, error) {
		return commit(files, branch)
	}
}

func TestReconcileAplOverlay_FillsAndOverlays(t *testing.T) {
	setAplOverlayEnv(t)
	shared := map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile):         clusterspec.RenderObjOverlayShared(),
		envOverlayPath("primary", clusterspec.OverlayObjFile): clusterspec.RenderObjOverlayEnv("primary", "us-ord-1"),
		// apps SOURCE: LLZ wants knative OFF. (bare apps: map — LLZ's desired-state input)
		sharedOverlayPath(clusterspec.OverlayAppsFile):         "apps:\n  knative:\n    enabled: false\n",
		envOverlayPath("primary", clusterspec.OverlayAppsFile): "",
		// apl-operator's CURRENT per-app CR on the TARGET branch: enabled + owned config.
		aplAppTarget("knative"): "kind: AplApp\nmetadata:\n  name: knative\nspec:\n  enabled: true\n  resources:\n    foo: bar\n",
	}
	var gotFiles map[string]string
	var gotBranch string
	fakeOverlaySeams(t,
		func(p string) string { return shared[p] },
		func() (string, bool, error) { return "AKID", true, nil },
		func(files map[string]string, branch string) (string, bool, error) {
			gotFiles, gotBranch = files, branch
			return "newsha", true, nil
		},
	)

	if err := reconcileAplOverlay(context.Background(), metrics.NewRegistry()); err != nil {
		t.Fatalf("reconcileAplOverlay: %v", err)
	}
	if gotBranch != "apl-primary" {
		t.Errorf("target branch = %q, want apl-primary (defaulted from REGION)", gotBranch)
	}
	obj, ok := gotFiles[aplOverlayTargets[clusterspec.OverlayObjFile]]
	if !ok {
		t.Fatalf("obj target not in overlay files: %v", keysOf(gotFiles))
	}
	// apps fanned out to the per-app AplApp CR: enabled flipped to LLZ's desired value,
	// apl-operator's owned config (resources) key-level-preserved.
	knative, ok := gotFiles[aplAppTarget("knative")]
	if !ok {
		t.Fatalf("per-app target %q not in overlay files: %v", aplAppTarget("knative"), keysOf(gotFiles))
	}
	if !strings.Contains(knative, "enabled: false") {
		t.Errorf("knative.yaml must have enabled flipped to false:\n%s", knative)
	}
	if !strings.Contains(knative, "foo: bar") {
		t.Errorf("knative.yaml must preserve apl-operator's owned config (resources):\n%s", knative)
	}
	// The accessKeyId placeholder was filled from OpenBao; the merged env
	// region/buckets are present.
	if strings.Contains(obj, clusterspec.ObjAccessKeyIDPlaceholder) {
		t.Errorf("obj.yaml still has the accessKeyId placeholder after fill:\n%s", obj)
	}
	if !strings.Contains(obj, "AKID") {
		t.Errorf("obj.yaml missing filled accessKeyId:\n%s", obj)
	}
	// The secret NEVER transits git: apl-core reads secretAccessKey from the
	// obj-secrets Secret via ESO, so it is ABSENT from the settings overlay
	// entirely. Guard against any regression that re-introduces it.
	if strings.Contains(obj, "secretAccessKey") {
		t.Errorf("obj.yaml must NOT carry secretAccessKey (ESO owns it):\n%s", obj)
	}
	// It is apl-core's AplObjectStorage settings CR, not a bare obj: map.
	if !strings.Contains(obj, "kind: AplObjectStorage") {
		t.Errorf("obj.yaml must be the AplObjectStorage CR:\n%s", obj)
	}
	if !strings.Contains(obj, "us-ord-1") || !strings.Contains(obj, "platform-loki-chunks-primary") {
		t.Errorf("obj.yaml missing merged env region/buckets:\n%s", obj)
	}
}

// When the obj credential is not seeded, obj.yaml is SKIPPED (never push a
// placeholder) but the app toggles still sync (they carry no secret).
func TestReconcileAplOverlay_SkipsObjWhenCredMissing(t *testing.T) {
	setAplOverlayEnv(t)
	shared := map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile):          clusterspec.RenderObjOverlayShared(),
		envOverlayPath("primary", clusterspec.OverlayObjFile):  clusterspec.RenderObjOverlayEnv("primary", "us-ord-1"),
		sharedOverlayPath(clusterspec.OverlayAppsFile):         "apps:\n  knative:\n    enabled: false\n",
		envOverlayPath("primary", clusterspec.OverlayAppsFile): "",
		aplAppTarget("knative"):                                "kind: AplApp\nmetadata:\n  name: knative\nspec:\n  enabled: true\n",
	}
	var gotFiles map[string]string
	fakeOverlaySeams(t,
		func(p string) string { return shared[p] },
		func() (string, bool, error) { return "", false, nil }, // not seeded
		func(files map[string]string, _ string) (string, bool, error) {
			gotFiles = files
			return "sha", true, nil
		},
	)
	if err := reconcileAplOverlay(context.Background(), metrics.NewRegistry()); err != nil {
		t.Fatalf("reconcileAplOverlay: %v", err)
	}
	if _, ok := gotFiles[aplOverlayTargets[clusterspec.OverlayObjFile]]; ok {
		t.Error("obj.yaml must be skipped when the credential is not seeded (no placeholder push)")
	}
	if _, ok := gotFiles[aplAppTarget("knative")]; !ok {
		t.Error("app toggles must still sync when the obj credential is missing")
	}
}

// A missing apl-<env> branch (apl-operator not bootstrapped yet) is a no-op, not
// an error.
func TestReconcileAplOverlay_MissingBranchIsNoOp(t *testing.T) {
	setAplOverlayEnv(t)
	fakeOverlaySeams(t,
		func(p string) string {
			if strings.HasSuffix(p, clusterspec.OverlayAppsFile) {
				return clusterspec.RenderAppsOverlayShared()
			}
			return ""
		},
		func() (string, bool, error) { return "AKID", true, nil },
		func(map[string]string, string) (string, bool, error) { return "", false, errGHRefNotFound },
	)
	if err := reconcileAplOverlay(context.Background(), metrics.NewRegistry()); err != nil {
		t.Errorf("missing target branch must be a no-op, got: %v", err)
	}
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
	orig := aplValuesRepoTokenFile
	aplValuesRepoTokenFile = "/nonexistent/llz-apl-values-token" // mounted file absent too
	t.Cleanup(func() { aplValuesRepoTokenFile = orig })
	if err := reconcileAplOverlay(context.Background(), metrics.NewRegistry()); err != nil {
		t.Errorf("unsynced token must be a no-op, got: %v", err)
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
