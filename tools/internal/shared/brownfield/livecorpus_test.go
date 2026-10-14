package brownfield

// livecorpus_test.go — the cluster states CI cannot build, kept as fixtures.
//
// THE GAP THIS CLOSES IS THE ONE docs/e2e-gates.md NAMES. Every e2e lane
// force-pushes a fresh instantiation at the commit under test, so the only
// configuration the release gate has ever exercised is greenfield: each object is
// CREATED in its final shape, and the failure this whole package exists for —
// a declared field that an object which PREDATES it cannot accept — is
// unreachable there by construction. The Terraform half of that gap is still open
// for want of a lane that can stand up older state.
//
// This is the cheap half of the answer: the older state already exists, in every
// adopter's cluster, so capture it and check it in. `loki-ingester.brownfield.json`
// is the real StatefulSet off gsap-apl prod (LKE 635371) on 2026-08-31, trimmed to
// the subtrees the field map walks plus the identity that makes it recognisable.
// It carries what the outage carried: generation 2, apl-core's own
// `{cpu: 500m, memory: 1Gi}`, an emptyDir named `data`, and NO
// volumeClaimTemplates.
//
// WHAT IT BUYS OVER A HAND-WRITTEN FIXTURE. A fixture written from the same
// mental model as the code tests the model, not the cluster — and three details
// here were not in that model. The limits are apl-core's own defaults rather than
// the shape the write-up assumed. One row, `requests.memory`, MATCHES the overlay
// by coincidence at 512Mi, so a green row on this object proves nothing (see
// capturedRowVerdicts). The WAL emptyDir is a pod volume NAMED `data`, the same
// name as the claim the overlay declares, so a presence check keying on the name
// rather than on volumeClaimTemplates would report the WAL durable on the very
// object whose WAL is not. And the object is at generation 2 — it HAS been updated
// in place since creation, which is what makes the immutable path surprising.
//
// REFRESHING IT is a `kubectl get -o json` and a trim, and it should be refreshed
// whenever apl-core changes the ingester's shape. A stale fixture here is not a
// silent pass: the rows are asserted individually, so a shape that no longer
// matches fails naming the field.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// liveObject loads a captured cluster object from the clusterspec corpus. The
// fixtures live beside the field map because that is what they are fixtures FOR;
// this package borrows them rather than keeping a second copy, so a refresh
// updates one file and both consumers see it.
func liveObject(t *testing.T, name string) map[string]any {
	t.Helper()
	path := filepath.Join("..", "clusterspec", "testdata", "live", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the captured object: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("%s does not decode: %v", name, err)
	}
	return obj
}

// ONE ROW MATCHES BY COINCIDENCE, AND THAT IS THE MOST INSTRUCTIVE THING HERE.
// The overlay asks for `requests.memory: 512Mi` and apl-core's own default IS
// 512Mi, so that row reads DELIVERED on an object where NOTHING the overlay
// declares has been applied. A green row is not evidence the overlay landed —
// it can agree with the chart default by accident, and four of the five rows
// disagreeing is what actually says the overlay never reached this object.
//
// It also corrects the write-up this PR was built from, which said none of the
// four resource values had landed. Four of five rows undelivered, not five: the
// difference is a fixture captured from the cluster rather than reasoned about.
var capturedRowVerdicts = map[string]bool{ // path -> delivered?
	"loki.ingester.resources.limits.memory":   false, // 1Gi live vs 3Gi declared
	"loki.ingester.resources.limits.cpu":      false, // 500m live vs 1 declared
	"loki.ingester.resources.requests.cpu":    false, // 250m live vs 100m declared
	"loki.ingester.resources.requests.memory": true,  // 512Mi both — apl-core's default
	"loki.ingester.persistence.enabled":       false, // no volumeClaimTemplates at all
}

// THE THREE CONSUMERS MUST AGREE ON ONE OBJECT. `assert-overlay-applied` decides
// whether to fail a lane, the migration decides whether to DELETE a live
// StatefulSet, and the reconciler lane decides what to publish — all three from
// clusterspec.OverlayFieldDelivered. They are wired separately and could drift
// apart without any single package's tests noticing; this walks the real object
// through the shared predicate row by row and pins every verdict.
func TestTheCapturedBrownfieldStatefulSetIsUndeliveredInEveryMappedRow(t *testing.T) {
	live := liveObject(t, "loki-ingester.brownfield.json")
	raw := clusterspec.AplAppRawValues()

	checked := 0
	for _, f := range clusterspec.OverlayFields() {
		if f.Name != "loki-ingester" {
			continue
		}
		path := clusterspec.OverlayFieldPath(f)
		declared, ok := clusterspec.RawValue(raw[f.App], f.Value...)
		if !ok {
			t.Fatalf("%s: the overlay declares no such path", path)
		}
		want, known := capturedRowVerdicts[path]
		if !known {
			t.Errorf("%s has no verdict recorded against the captured object — a row was added without "+
				"anyone checking what it says about the state this package exists for", path)
			continue
		}
		match, delivered, readable := clusterspec.OverlayFieldDelivered(f, declared, live)
		if !readable {
			t.Errorf("%s does not resolve on the REAL object (%s) — the row points at something this "+
				"cluster does not have, so it covers nothing", path, delivered)
			continue
		}
		checked++
		if match != want {
			t.Errorf("%s: delivered=%v against the captured object, want %v (declared %v, live says %q)",
				path, match, want, declared, delivered)
		}
	}
	if checked != len(capturedRowVerdicts) {
		t.Fatalf("checked %d rows against the captured object, want %d — a row was added or removed "+
			"without this corpus being revisited", checked, len(capturedRowVerdicts))
	}
}

// THE emptyDir IS NAMED `data`, and so is the claim the overlay declares. A
// presence check written against pod volumes rather than volumeClaimTemplates
// would find it and report the WAL as durable — on the exact object whose
// non-durable WAL is the outage. Pinned because the two names being equal is not
// a coincidence anyone would notice while writing the simpler check.
func TestTheEmptyDirSharesTheClaimName(t *testing.T) {
	live := liveObject(t, "loki-ingester.brownfield.json")
	vols, _, _ := clusterspec.LiveValue(live, []string{"spec", "template", "spec", "volumes"})
	list, _ := vols.([]any)
	var found bool
	for _, v := range list {
		m, _ := v.(map[string]any)
		if m["name"] != clusterspec.LokiWALClaimName {
			continue
		}
		found = true
		if _, isEmptyDir := m["emptyDir"]; !isEmptyDir {
			t.Errorf("the %q volume in the captured object is not an emptyDir any more — refresh the "+
				"fixture, and check whether the trap it captures still exists", clusterspec.LokiWALClaimName)
		}
	}
	if !found {
		t.Fatalf("no volume named %q in the captured object — this test is not reaching what it describes",
			clusterspec.LokiWALClaimName)
	}
	// …and the durability question is answered by the claim templates, not by that.
	if _, _, ok := clusterspec.LiveValue(live, []string{"spec", "volumeClaimTemplates"}); ok {
		t.Error("the captured object has volumeClaimTemplates — it is no longer the pre-migration shape")
	}
}

// The migration engine reads the same object and must call it PENDING: a state
// the gate fails on and the migration calls done would leave an operator with a
// red lane and no remedy.
func TestTheCapturedObjectReadsPendingToTheMigrationEngine(t *testing.T) {
	live := liveObject(t, "loki-ingester.brownfield.json")
	raw, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	withObject(t, func() (string, bool, bool) { return string(raw), false, true })

	sts := MigrationStatuses(testDeps())
	if len(sts) != 1 {
		t.Fatalf("statuses = %d, want 1", len(sts))
	}
	if sts[0].State != MigrationPending {
		t.Errorf("state = %s, want PENDING on the object the migration was written for: %s",
			sts[0].State, sts[0].Detail)
	}
	if sts[0].Migration.ID != clusterspec.LokiWALPVCMigration {
		t.Errorf("migration = %s, want %s", sts[0].Migration.ID, clusterspec.LokiWALPVCMigration)
	}
}

// AND THE GREENFIELD SHAPE MUST NOT. The same fixture with claim templates added
// is what CI actually builds, and the migration has to be a no-op there — an
// eager repair that fires on every fresh cluster would recreate a StatefulSet on
// every bootstrap for no reason.
func TestTheGreenfieldShapeOfTheSameObjectIsDone(t *testing.T) {
	live := liveObject(t, "loki-ingester.brownfield.json")
	spec := live["spec"].(map[string]any)
	spec["volumeClaimTemplates"] = []any{map[string]any{
		"metadata": map[string]any{"name": clusterspec.LokiWALClaimName},
	}}
	raw, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	withObject(t, func() (string, bool, bool) { return string(raw), false, true })

	if got := MigrationStatuses(testDeps())[0].State; got != MigrationDone {
		t.Errorf("state = %s, want DONE — an object created in its final shape has nothing to migrate", got)
	}
}

// THE GUARD DEPENDS ON A SHAPE apl-core OWNS, so the shape is checked in. Before
// this, nothing in-tree pinned WHERE the owning Application carries its values:
// `valueDocs` reads `helm.values`, `helm.valuesObject` and `helm.parameters`, and
// an Application that used `helm.valueFiles` instead would yield no document —
// at which point ownerCanRecreate defers forever, converge exits 0 having
// repaired nothing, and even `--yes` refuses. Silently inert, which is the
// failure this package exists to end.
//
// The fixture is the real monitoring-loki Application off gsap-apl prod with its
// 7KB values document verbatim, not trimmed: trimming it would remove the very
// thing being pinned.
func TestTheRealApplicationCarriesItsValuesWhereTheGuardLooks(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "extensions", "assertions", "assertplatform",
		"testdata", "monitoring-loki.synced-no-conditions.json"))
	if err != nil {
		t.Fatalf("reading the captured Application: %v", err)
	}
	var policy ownerPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatalf("the captured Application does not decode: %v", err)
	}
	docs := policy.valueDocs()
	if len(docs) == 0 {
		t.Fatalf("the REAL Application carries no values this guard can read (%v) — every migration would "+
			"defer forever and the repair would be silently inert", policy.unreadableSources())
	}
	// …and the value the migration is about is findable in it.
	for _, f := range clusterspec.OverlayFields() {
		if !f.CreateOnly {
			continue
		}
		got, ok := desiredValue(docs, f.Value)
		if !ok {
			t.Errorf("%s is not resolvable in the real Application's values — the guard would refuse to "+
				"migrate it on the very cluster it was written for", clusterspec.OverlayFieldPath(f))
			continue
		}
		want, _ := clusterspec.RawValue(clusterspec.AplAppRawValues()[f.App], f.Value...)
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
			t.Errorf("%s: the Application renders %v and the overlay declares %v",
				clusterspec.OverlayFieldPath(f), got, want)
		}
	}
}
