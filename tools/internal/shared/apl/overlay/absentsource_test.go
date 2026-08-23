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
		{"a CR with no region or buckets", clusterspec.RenderObjOverlayShared()},
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

// THE REFUSAL IS VISIBLE TO THE ONLY THING THAT WATCHES THIS LANE. It first
// printed `::warning::`, a GitHub Actions instruction, from code that runs in a
// pod on a reconcile loop — where nothing reads stdout for annotations and there
// is no job summary. The condition was invisible to Prometheus, which is the
// whole audience.
func TestAnEmptyObjSourceIsVisibleAsAGauge(t *testing.T) {
	reg := metrics.NewRegistry()
	repo := &fakeRepo{files: map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile): "{}\n",
	}}
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, reg); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := renderedMetrics(t, reg)
	if !strings.Contains(got, "llz_apl_overlay_obj_source_empty") {
		t.Fatalf("the refusal published no series:\n%s", got)
	}
	if !strings.Contains(got, `llz_apl_overlay_obj_source_empty{branch="apl-primary"} 1`) {
		t.Errorf("want the gauge at 1 for an empty source:\n%s", got)
	}
}

// AND IT GOES BACK TO 0, rather than ceasing to exist. An alert on an absent
// series never evaluates, so a gauge that only appears when things are wrong
// cannot be alerted on at all — and the pass that FIXES the overlay is exactly
// when the series would have vanished.
func TestTheEmptyObjSourceGaugeIsPublishedOnAHealthyPass(t *testing.T) {
	reg := metrics.NewRegistry()
	repo := &fakeRepo{files: map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile):         clusterspec.RenderObjOverlayShared(),
		envOverlayPath("primary", clusterspec.OverlayObjFile): clusterspec.RenderObjOverlayEnv("acme", "primary", "us-ord-1"),
	}}
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, reg); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := renderedMetrics(t, reg); !strings.Contains(got, `llz_apl_overlay_obj_source_empty{branch="apl-primary"} 0`) {
		t.Errorf("the gauge must be published at 0 on a healthy pass:\n%s", got)
	}
}

// A PASS THAT FOUND NOTHING IS DISTINGUISHABLE FROM A HEALTHY ONE. The skips are
// correct, but "correctly nothing to do" and "reading the wrong branch" produce
// the identical outcome — no error, no files, and llz_apl_overlay_synced never
// published because it has no 0 arm and the pass returns before it. A misspelled
// Env or SourceBranch, or a token GitHub 404s on a private repo, lands there.
func TestAPassWithNoSourceIsVisibleAsAGauge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"no source at all", map[string]string{}, "0"},
		// THE ROW THAT MAKES THE ALERT REAL. `_shared` has no env in its path, so
		// a misspelled REGION still reads it — keyed on "any layer found" the
		// gauge stayed at 1 and LLZAplOverlayNoSource could never fire for the
		// case it is named after.
		{"shared layer only, this env absent", map[string]string{
			sharedOverlayPath(clusterspec.OverlayObjFile): clusterspec.RenderObjOverlayShared(),
		}, "0"},
		{"this env's apps layer present", map[string]string{
			sharedOverlayPath(clusterspec.OverlayAppsFile):         "apps:\n  knative:\n    enabled: false\n",
			envOverlayPath("primary", clusterspec.OverlayAppsFile): "apps:\n  loki:\n    enabled: true\n",
		}, "1"},
		{"this env's obj layer present", map[string]string{
			sharedOverlayPath(clusterspec.OverlayObjFile):         clusterspec.RenderObjOverlayShared(),
			envOverlayPath("primary", clusterspec.OverlayObjFile): clusterspec.RenderObjOverlayEnv("acme", "primary", "us-ord-1"),
		}, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := metrics.NewRegistry()
			repo := &fakeRepo{files: tc.files}
			if err := Reconcile(context.Background(), testCfg(), repo, credsOK, reg); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			got := renderedMetrics(t, reg)
			want := `llz_apl_overlay_source_present{branch="main",env="primary"} ` + tc.want
			if !strings.Contains(got, want) {
				t.Errorf("want %q in:\n%s", want, got)
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

// A _shared-ONLY OBJ OVERLAY IS REFUSED, AND THIS TEST USED TO REQUIRE THE
// OPPOSITE. It asserted "one layer is enough" on the reasoning that `found` is
// the OR of the two reads — true, and beside the point: the _shared layer
// carries kind, metadata, showWizard and the accessKeyId placeholder, and NO
// region and NO buckets, because supplying those is the per-env layer's entire
// job. Writing it wholesale blanks both on apl-core's live CR, which is the same
// loki/harbor outage as the `{}` case by a narrower route. The gate pinned the
// bug as intended behaviour.
//
// The bar is CONTENT, not layer count — see the next test.
func TestASharedOnlyObjOverlayIsRefusedBecauseItCarriesNoRegionOrBuckets(t *testing.T) {
	repo := &fakeRepo{files: map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile): clusterspec.RenderObjOverlayShared(),
	}}
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got, ok := repo.gotFiles[objTarget()]; ok {
		t.Errorf("a region-less, bucket-less obj overlay was written over apl-core's CR:\n%s", got)
	}
}

// AND CONTENT IS THE BAR, so a hand-written _shared layer that DOES carry region
// and buckets is written — the rule is about what the document says, not about
// which file it came from.
func TestACompleteObjOverlayInOneLayerIsWritten(t *testing.T) {
	complete := "kind: AplObjectStorage\nmetadata:\n  name: obj\nspec:\n  provider:\n    type: linode\n" +
		"    linode:\n      accessKeyId: " + clusterspec.ObjAccessKeyIDPlaceholder + "\n" +
		"      region: us-ord-1\n      buckets:\n        loki: acme-loki\n        harbor: acme-harbor\n"
	repo := &fakeRepo{files: map[string]string{
		sharedOverlayPath(clusterspec.OverlayObjFile): complete,
	}}
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got, ok := repo.gotFiles[objTarget()]
	if !ok {
		t.Fatal("a complete single-layer obj overlay must be written")
	}
	if !strings.Contains(got, "us-ord-1") || !strings.Contains(got, "acme-loki") {
		t.Errorf("written obj.yaml lost its substance:\n%s", got)
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
