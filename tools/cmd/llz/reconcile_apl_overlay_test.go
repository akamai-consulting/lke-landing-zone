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
func fakeOverlaySeams(t *testing.T, read func(path string) string, creds func() (string, string, bool, error), commit func(files map[string]string, branch string) (string, bool, error)) {
	t.Helper()
	origRead, origCommit, origCreds := aplOverlayReadFileFn, aplOverlayCommitFn, aplOverlayObjCredsFn
	t.Cleanup(func() { aplOverlayReadFileFn, aplOverlayCommitFn, aplOverlayObjCredsFn = origRead, origCommit, origCreds })

	aplOverlayReadFileFn = func(_ context.Context, _ *http.Client, _, _, _, path string) (string, bool, error) {
		c := read(path)
		return c, c != "", nil
	}
	aplOverlayObjCredsFn = func(_ context.Context, _ string) (string, string, bool, error) { return creds() }
	aplOverlayCommitFn = func(_ context.Context, _ *http.Client, _, _, branch string, files map[string]string, _ string, _ int) (string, bool, error) {
		return commit(files, branch)
	}
}

func TestReconcileAplOverlay_FillsAndOverlays(t *testing.T) {
	setAplOverlayEnv(t)
	shared := map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile):  clusterspec.RenderObjOverlayShared(),
		sharedOverlayPath(clusterspec.OverlayAppsFile): clusterspec.RenderAppsOverlayShared(),
		envOverlayPath("primary", clusterspec.OverlayObjFile):  clusterspec.RenderObjOverlayEnv("primary", "us-ord-1"),
		envOverlayPath("primary", clusterspec.OverlayAppsFile): clusterspec.RenderAppsOverlayEnv(map[string]clusterspec.ComponentToggle{"observability": {}}),
	}
	var gotFiles map[string]string
	var gotBranch string
	fakeOverlaySeams(t,
		func(p string) string { return shared[p] },
		func() (string, string, bool, error) { return "AKID", "SECRET", true, nil },
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
	// Both owned files were mapped to their apl-<env> tree paths.
	objTree := aplOverlayTargets[clusterspec.OverlayObjFile]
	appsTree := aplOverlayTargets[clusterspec.OverlayAppsFile]
	obj, ok := gotFiles[objTree]
	if !ok {
		t.Fatalf("obj target %q not in overlay files: %v", objTree, keysOf(gotFiles))
	}
	if _, ok := gotFiles[appsTree]; !ok {
		t.Errorf("apps target %q not in overlay files", appsTree)
	}
	// The obj credential was filled (placeholders gone, real values in) and the
	// merged env region/buckets are present.
	if strings.Contains(obj, clusterspec.ObjAccessKeyIDPlaceholder) || strings.Contains(obj, clusterspec.ObjSecretAccessKeyPlaceholder) {
		t.Errorf("obj.yaml still has placeholders after fill:\n%s", obj)
	}
	if !strings.Contains(obj, "AKID") || !strings.Contains(obj, "SECRET") {
		t.Errorf("obj.yaml missing filled creds:\n%s", obj)
	}
	if !strings.Contains(obj, "us-ord-1") || !strings.Contains(obj, "platform-loki-chunks-primary") {
		t.Errorf("obj.yaml missing merged env region/buckets:\n%s", obj)
	}
}

// When the obj credential is not seeded, obj.yaml is SKIPPED (never push a
// placeholder) but apps.yaml still syncs.
func TestReconcileAplOverlay_SkipsObjWhenCredMissing(t *testing.T) {
	setAplOverlayEnv(t)
	shared := map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile):          clusterspec.RenderObjOverlayShared(),
		sharedOverlayPath(clusterspec.OverlayAppsFile):         clusterspec.RenderAppsOverlayShared(),
		envOverlayPath("primary", clusterspec.OverlayObjFile):  clusterspec.RenderObjOverlayEnv("primary", "us-ord-1"),
		envOverlayPath("primary", clusterspec.OverlayAppsFile): clusterspec.RenderAppsOverlayEnv(nil),
	}
	var gotFiles map[string]string
	fakeOverlaySeams(t,
		func(p string) string { return shared[p] },
		func() (string, string, bool, error) { return "", "", false, nil }, // not seeded
		func(files map[string]string, _ string) (string, bool, error) { gotFiles = files; return "sha", true, nil },
	)
	if err := reconcileAplOverlay(context.Background(), metrics.NewRegistry()); err != nil {
		t.Fatalf("reconcileAplOverlay: %v", err)
	}
	if _, ok := gotFiles[aplOverlayTargets[clusterspec.OverlayObjFile]]; ok {
		t.Error("obj.yaml must be skipped when the credential is not seeded (no placeholder push)")
	}
	if _, ok := gotFiles[aplOverlayTargets[clusterspec.OverlayAppsFile]]; !ok {
		t.Error("apps.yaml must still sync when the obj credential is missing")
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
		func() (string, string, bool, error) { return "AKID", "SECRET", true, nil },
		func(map[string]string, string) (string, bool, error) { return "", false, errGHRefNotFound },
	)
	if err := reconcileAplOverlay(context.Background(), metrics.NewRegistry()); err != nil {
		t.Errorf("missing target branch must be a no-op, got: %v", err)
	}
}

// Missing required env → a loud misconfiguration error (not a silent no-op).
func TestReconcileAplOverlay_MisconfigErrors(t *testing.T) {
	t.Setenv("GH_REPO", "")
	t.Setenv("APL_VALUES_REPO_TOKEN", "")
	t.Setenv("REGION", "")
	if err := reconcileAplOverlay(context.Background(), metrics.NewRegistry()); err == nil {
		t.Error("missing GH_REPO/APL_VALUES_REPO_TOKEN/REGION must error")
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
