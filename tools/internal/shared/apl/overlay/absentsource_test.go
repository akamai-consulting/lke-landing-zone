package overlay

// absentsource_test.go — an overlay with no source must write nothing.
//
// The reconciler's job is to overlay LLZ's opinions onto the machine-owned
// apl-<env> branch, which is apl-core's OWN configuration. Everything it writes
// there replaces something apl-core put there, so "LLZ has no opinion" and "LLZ
// says empty" have to be different answers — and they were the same one.
//
// repo.ReadFile answers a missing path with ("", false, nil). readMergedOverlay
// discarded both `found` flags, so two absent layers reached
// MergeOverlay([]byte(""), []byte("")) — which does not return empty. It returns
// "{}\n", the canonical YAML for an empty map. The caller's
// `len(bytes.TrimSpace(objMerged)) > 0` guard therefore passed, and `{}` went
// onto the branch over apl-core's live AplObjectStorage CR. Object storage for
// loki and harbor stops resolving; the commit succeeds and the lane reports
// synced.
//
// The existing no-obj-source case in this package could not see it: it uses
// credsMissing, which takes the credential skip branch before the write.

import (
	"context"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
)

// objTarget is the path on the machine branch the obj overlay writes to.
func objTarget() string { return aplOverlayTargets[clusterspec.OverlayObjFile] }

// THE PROBE FROM THE REVIEW. No obj overlay source at all, and — crucially — a
// credential that IS seeded, so nothing short of the found flag stops the write.
func TestAnAbsentObjSourceWritesNothingRatherThanBlankingTheCR(t *testing.T) {
	repo := &fakeRepo{files: map[string]string{
		// Apps source present so the pass still commits: the failure being tested
		// is a file written, not a pass that ran.
		sharedOverlayPath(clusterspec.OverlayAppsFile): "apps:\n  knative:\n    enabled: false\n",
		aplAppTarget("knative"):                        "kind: AplApp\nmetadata:\n  name: knative\nspec:\n  enabled: true\n",
	}}

	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got, ok := repo.gotFiles[objTarget()]; ok {
		t.Fatalf("wrote %s with no overlay source to write from:\n%q\n\n"+
			"That path is apl-core's live AplObjectStorage CR. An absent source means LLZ has "+
			"no opinion; writing an empty document means LLZ says there is no object storage.",
			objTarget(), got)
	}
}

// AND `{}` IS NOT A LEGITIMATE PAYLOAD EITHER, from a source that does exist.
// That is a different fault — a render bug rather than an un-rendered instance —
// with the identical consequence if written, so it also skips, and loudly.
func TestAnEmptyObjSourceIsRefusedRatherThanWritten(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"empty map", "{}\n"},
		{"document separator only", "---\n{}\n"},
		{"comments only", "# nothing here yet\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{files: map[string]string{
				sharedOverlayPath(clusterspec.OverlayObjFile): tc.content,
			}}
			if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got, ok := repo.gotFiles[objTarget()]; ok {
				t.Errorf("an overlay source that merges to nothing was written to %s:\n%q", objTarget(), got)
			}
		})
	}
}

// THE POSITIVE CASE, so the refusals above cannot be satisfied by a reconciler
// that stopped writing obj altogether — which would be the same outage arriving
// by a different route.
func TestARealObjSourceIsStillWritten(t *testing.T) {
	repo := &fakeRepo{files: map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile):         clusterspec.RenderObjOverlayShared(),
		envOverlayPath("primary", clusterspec.OverlayObjFile): clusterspec.RenderObjOverlayEnv("acme", "primary", "us-ord-1"),
	}}
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got, ok := repo.gotFiles[objTarget()]
	if !ok {
		t.Fatal("a real obj overlay source produced no write")
	}
	for _, want := range []string{"AplObjectStorage", "AKID", "us-ord-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("written obj.yaml missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, clusterspec.ObjAccessKeyIDPlaceholder) {
		t.Errorf("the accessKeyId placeholder reached the machine branch:\n%s", got)
	}
}

// ONE LAYER IS ENOUGH. `found` is the OR of the two reads, not the AND — an
// instance whose per-env layer has not been rendered yet still has a _shared
// base, and refusing that would be the same silence for a different reason.
func TestOneOverlayLayerIsEnoughToWrite(t *testing.T) {
	repo := &fakeRepo{files: map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile): clusterspec.RenderObjOverlayShared(),
	}}
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := repo.gotFiles[objTarget()]; !ok {
		t.Error("a _shared-only obj overlay must still be written; env layers are optional")
	}
}

// THE APPS PASS ANSWERS THE SAME QUESTION THE SAME WAY. It writes per-app files
// rather than one document, so an absent source was already harmless — this pins
// that the two passes cannot drift into disagreeing about what absent means.
func TestAnAbsentAppsSourceWritesNoAppFiles(t *testing.T) {
	repo := &fakeRepo{files: map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile): clusterspec.RenderObjOverlayShared(),
		// An AplApp already on the branch, so a pass that decided to write would
		// have something to overwrite.
		aplAppTarget("knative"): "kind: AplApp\nmetadata:\n  name: knative\nspec:\n  enabled: true\n",
	}}
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got, ok := repo.gotFiles[aplAppTarget("knative")]; ok {
		t.Errorf("wrote an AplApp with no apps overlay source:\n%q", got)
	}
}
