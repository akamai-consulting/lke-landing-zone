package assertplatform

// overlay_applied_test.go covers the two judgements this gate makes — is the
// value delivered, and if not is it even appliable — plus every fail-closed arm.
//
// THE LIVE OBJECTS ARE THE REAL SHAPES. loki-ingester's JSON below is the shape
// the cluster actually returned during the outage this gate was written for: 1Gi,
// an emptyDir, and no volumeClaimTemplates. A fixture invented to match the
// walker would test the walker against itself.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// the brownfield shape: the object apl-core created before the overlay existed.
const brownfieldIngester = `{
  "apiVersion": "apps/v1", "kind": "StatefulSet",
  "metadata": {"name": "loki-ingester", "namespace": "monitoring", "generation": 2},
  "spec": {
    "template": {"spec": {
      "containers": [{"name": "ingester", "resources": {"limits": {"cpu": "1", "memory": "1Gi"}}}],
      "volumes": [{"name": "data", "emptyDir": {}}]
    }}
  }}`

// the converged shape: what the overlay asks for.
const deliveredIngester = `{
  "apiVersion": "apps/v1", "kind": "StatefulSet",
  "metadata": {"name": "loki-ingester", "namespace": "monitoring"},
  "spec": {
    "template": {"spec": {
      "containers": [{"name": "ingester", "resources": {
        "limits": {"cpu": "1", "memory": "3Gi"},
        "requests": {"cpu": "100m", "memory": "512Mi"}
      }}]
    }},
    "volumeClaimTemplates": [{"metadata": {"name": "data"}}]
  }}`

// the real refusal, verbatim from the apiserver.
const statefulSetRefusal = `The StatefulSet "loki-ingester" is invalid: spec: Forbidden: updates to ` +
	`statefulset spec for fields other than 'replicas', 'ordinals', 'template', 'updateStrategy', ` +
	`'revisionHistoryLimit', 'persistentVolumeClaimRetentionPolicy' and 'minReadySeconds' are forbidden`

func withLiveObject(t *testing.T, raw string, absent, answered bool) {
	t.Helper()
	prev := readLiveObject
	readLiveObject = func(string, string, string) ([]byte, bool, bool, string) {
		return []byte(raw), absent, answered, "stubbed kubectl said nothing"
	}
	t.Cleanup(func() { readLiveObject = prev })
}

// withDryRun records the patches the gate sends and answers with a fixed verdict.
func withDryRun(t *testing.T, out string, accepted bool) *[]string {
	t.Helper()
	var sent []string
	prev := dryRunPatch
	dryRunPatch = func(_, _, _, patch string) (string, bool) {
		sent = append(sent, patch)
		return out, accepted
	}
	t.Cleanup(func() { dryRunPatch = prev })
	return &sent
}

func loadObj(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("fixture does not decode: %v", err)
	}
	return m
}

func fieldNamed(t *testing.T, path string) clusterspec.OverlayField {
	t.Helper()
	for _, f := range clusterspec.OverlayFields() {
		if clusterspec.OverlayFieldPath(f) == path {
			return f
		}
	}
	t.Fatalf("no overlay field %q in the map", path)
	return clusterspec.OverlayField{}
}

// ── reading the value back ───────────────────────────────────────────────────

func TestAScalarIsComparedAgainstTheContainerTheChartNames(t *testing.T) {
	f := fieldNamed(t, "loki.ingester.resources.limits.memory")
	match, delivered, found := clusterspec.OverlayFieldDelivered(f, "3Gi", loadObj(t, brownfieldIngester))
	if !found {
		t.Fatal("the memory limit is right there in the fixture; the walker did not reach it")
	}
	if match {
		t.Error("1Gi delivered against 3Gi declared must not match")
	}
	if delivered != "1Gi" {
		t.Errorf("the report must carry what is ACTUALLY there, got %q", delivered)
	}
}

func TestAPresenceMatchReadsTheListTheToggleRenders(t *testing.T) {
	f := fieldNamed(t, "loki.ingester.persistence.enabled")
	// The brownfield StatefulSet has no volumeClaimTemplates key at all. That is the
	// FINDING — zero claim templates against a declared true — and grading it as a
	// path the gate could not read would report the probe as broken instead.
	match, delivered, found := clusterspec.OverlayFieldDelivered(f, true, loadObj(t, brownfieldIngester))
	if !found {
		t.Fatal("an absent list is an answer (zero entries), not a failure to ask")
	}
	if match {
		t.Error("zero claim templates does not satisfy persistence.enabled=true")
	}
	if delivered != "0 entries" {
		t.Errorf("the report must say what is there, got %q", delivered)
	}
	match, _, found = clusterspec.OverlayFieldDelivered(f, true, loadObj(t, deliveredIngester))
	if !found || !match {
		t.Error("a StatefulSet carrying claim templates satisfies persistence.enabled=true")
	}
}

// A chart rename must surface. The selector deliberately has no "if there is only
// one container, use it" fallback: that would match whatever happened to be there.
func TestARenamedContainerIsReportedRatherThanMatchedLoosely(t *testing.T) {
	renamed := strings.Replace(brownfieldIngester, `"name": "ingester"`, `"name": "loki"`, 1)
	f := fieldNamed(t, "loki.ingester.resources.limits.memory")
	if _, _, readable := clusterspec.OverlayFieldDelivered(f, "3Gi", loadObj(t, renamed)); readable {
		t.Error("a renamed container must read as a row this gate can no longer resolve, not as a silent match")
	}
}

// The other half of that distinction: a key the object simply does not carry is a
// VALUE finding, and must go on to the appliability probe rather than being
// reported as a probe someone has to fix.
func TestAnAbsentKeyIsUndeliveredRatherThanUnreadable(t *testing.T) {
	f := fieldNamed(t, "loki.ingester.resources.requests.memory")
	match, delivered, readable := clusterspec.OverlayFieldDelivered(f, "512Mi", loadObj(t, brownfieldIngester))
	if !readable {
		t.Fatal("a container with no requests block at all is an object missing a value, " +
			"not a row pointing at nothing")
	}
	if match {
		t.Error("an absent request cannot match a declared one")
	}
	if delivered != "(absent)" {
		t.Errorf("the report must say the value is not there, got %q", delivered)
	}
}

// ── the second question: is the difference even appliable ────────────────────

func TestTheStatefulSetRefusalIsReportedAsUnappliableWithItsMigration(t *testing.T) {
	f := fieldNamed(t, "loki.ingester.persistence.enabled")
	v := classifyRefusal(f, statefulSetRefusal, false)
	if v.State != stateUnappliable {
		t.Fatalf("state = %s, want UNAPPLIABLE", v.State)
	}
	for _, want := range []string{"fixes this field at CREATE time", "discards every other change", f.Migration} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("the unappliable line must carry %q, got:\n%s", want, v.Detail)
		}
	}
}

func TestAnAcceptedDryRunIsUndeliveredRatherThanUnappliable(t *testing.T) {
	f := fieldNamed(t, "loki.ingester.resources.limits.memory")
	v := classifyRefusal(f, "statefulset.apps/loki-ingester patched (server dry run)", true)
	if v.State != stateNotApplied {
		t.Fatalf("state = %s, want APPLIABLE, NOT APPLIED", v.State)
	}
	// The contagion is the whole reason a mutable field can sit undelivered, so the
	// line has to offer it as a reading rather than leaving "not delivered" bare.
	if !strings.Contains(v.Detail, "another field on this same object is unappliable") {
		t.Errorf("the undelivered line must name the per-object diff as a candidate cause, got:\n%s", v.Detail)
	}
}

func TestAnUnclassifiedRefusalIsNotCalledImmutability(t *testing.T) {
	f := fieldNamed(t, "loki.ingester.resources.limits.memory")
	v := classifyRefusal(f, `Error from server (Forbidden): User "x" cannot patch resource "statefulsets"`, false)
	if v.State != stateRefused {
		t.Fatalf("an RBAC refusal must not be graded as immutability; state = %s", v.State)
	}
}

// ── the lane, end to end ─────────────────────────────────────────────────────

func TestTheBrownfieldShapeFailsAndSendsTheRealPatches(t *testing.T) {
	withLiveObject(t, brownfieldIngester, false, true)
	sent := withDryRun(t, statefulSetRefusal, false)
	if err := assertOverlayApplied(); err == nil {
		t.Fatal("a 1Gi emptyDir ingester under a 3Gi PVC overlay must fail the lane")
	}
	// Four: the three resource values the brownfield object does not carry (the cpu
	// LIMIT already matches at 1) plus the claim template. The mutable three are the
	// contagion — each would be applied happily on its own.
	if len(*sent) != 4 {
		t.Fatalf("every undelivered field must be probed, got %d patch(es): %v", len(*sent), *sent)
	}
	// The probe has to send the OBJECT's shape, not the chart's — a chart-shaped
	// claim would be rejected as malformed, which looks exactly like the
	// immutability rejection this gate exists to detect.
	joined := strings.Join(*sent, "\n")
	for _, want := range []string{`"volumeClaimTemplates"`, `"kind":"PersistentVolumeClaim"`,
		`"storageClassName":"block-storage-retain"`, `"memory":"3Gi"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("the appliability probe must carry %s; sent:\n%s", want, joined)
		}
	}
}

func TestTheConvergedShapePasses(t *testing.T) {
	withLiveObject(t, deliveredIngester, false, true)
	sent := withDryRun(t, "", true)
	if err := assertOverlayApplied(); err != nil {
		t.Fatalf("an object carrying what the overlay declares must pass, got %v", err)
	}
	if len(*sent) != 0 {
		t.Errorf("nothing should be probed when every value is already delivered, sent %v", *sent)
	}
}

// ── fail-closed arms ─────────────────────────────────────────────────────────

func TestAnUnreadableApiserverFailsTheOverlayLane(t *testing.T) {
	withLiveObject(t, "", false, false)
	withDryRun(t, "", true)
	if err := assertOverlayApplied(); err == nil {
		t.Fatal("could-not-read must fail: the lane cannot vouch for a value it never read")
	}
}

// NOTHING TO CHECK IS NOT THE SAME AS COULD NOT CHECK, and this lane is gating
// for every instance. Every mapped row points at one StatefulSet today, so on an
// instance that does not run the observability component the absent branch IS the
// whole run — failing it would be a gate nobody can turn green, which is the
// argument the harbor exemption makes one file over.
func TestAnInstanceThatDoesNotRunTheAppPassesLoudly(t *testing.T) {
	withLiveObject(t, "", true, true)
	withOwnerExists(t, false)
	sent := withDryRun(t, "", true)
	out := captureOverlayReport(t, assertOverlayApplied)
	if !strings.Contains(out, "Nothing to check on this cluster") {
		t.Errorf("the pass must say it examined nothing and why:\n%s", out)
	}
	if len(*sent) != 0 {
		t.Errorf("nothing may be probed when no object exists, got %v", *sent)
	}
}

// The other arm of the same branch: an object the gate could not READ is still a
// failure, because that is where it genuinely cannot speak.
func TestAnUnreadableObjectStillFailsOnVacuity(t *testing.T) {
	withLiveObject(t, "", false, false)
	withDryRun(t, "", true)
	err := assertOverlayApplied()
	if err == nil {
		t.Fatal("a run whose objects could not be read must not pass")
	}
	if !strings.Contains(err.Error(), "no overlay field examined") {
		t.Errorf("the vacuity failure must say so, got %v", err)
	}
}

func TestAnUndecodableObjectFails(t *testing.T) {
	withLiveObject(t, `{"spec":`, false, true)
	withDryRun(t, "", true)
	if err := assertOverlayApplied(); err == nil {
		t.Fatal("an object that does not decode is a field this gate cannot speak for")
	}
}

// ── coverage is reported, never assumed ──────────────────────────────────────

func TestUncheckedPathsAreTheDeclaredOnesWithNoRow(t *testing.T) {
	unchecked := uncheckedPaths(clusterspec.OverlayFields())
	if len(unchecked) == 0 {
		t.Skip("every declared path is mapped — nothing to report as unchecked")
	}
	mapped := map[string]bool{}
	for _, f := range clusterspec.OverlayFields() {
		mapped[clusterspec.OverlayFieldPath(f)] = true
	}
	for _, p := range unchecked {
		if mapped[p] {
			t.Errorf("%s has a row and must not be reported as unchecked", p)
		}
	}
	// Derived from the renderer, so a new overlay entry appears here the moment it
	// compiles rather than when someone remembers to add it.
	declared := map[string]bool{}
	for _, p := range clusterspec.DeclaredOverlayPaths() {
		declared[p] = true
	}
	for _, p := range unchecked {
		if !declared[p] {
			t.Errorf("%s is reported as unchecked but the overlay does not declare it", p)
		}
	}
}

// An app this instance does not run has no object, which must not fail the lane —
// and must not read as "all delivered" either. Today every row points at ONE
// object, so a permanently-missing one trips the examined==0 guard by accident;
// the moment a second object is mapped that accident stops holding, and the
// summary line is where the distinction would be lost.
func TestAnAbsentObjectIsCountedInTheSummaryRatherThanReadingAsDelivered(t *testing.T) {
	absent := overlayVerdict{
		Field: clusterspec.OverlayField{Kind: "statefulset", Namespace: "monitoring", Name: "loki-ingester"},
		State: stateObjectAbsent,
	}
	delivered := overlayVerdict{
		Field: clusterspec.OverlayField{App: "other", Value: []string{"x"}, Kind: "service",
			Namespace: "harbor", Name: "harbor-core"},
		State: stateDelivered,
	}
	out := captureOverlayReport(t, func() error {
		return reportOverlay(
			[]overlayVerdict{delivered, absent}, nil, nil, 1)
	})
	if strings.Contains(out, "All 1 mapped overlay field(s) are delivered") {
		t.Errorf("a run with an absent object must not claim everything was delivered:\n%s", out)
	}
	for _, want := range []string{"NOTHING was checked", "statefulset/monitoring/loki-ingester"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary must name what went unchecked; %q missing from:\n%s", want, out)
		}
	}
}

func TestAFullyDeliveredRunStillSaysSoPlainly(t *testing.T) {
	v := overlayVerdict{Field: clusterspec.OverlayField{App: "loki", Value: []string{"x"}}, State: stateDelivered}
	out := captureOverlayReport(t, func() error { return reportOverlay([]overlayVerdict{v}, nil, nil, 1) })
	if !strings.Contains(out, "All 1 mapped overlay field(s) are delivered") {
		t.Errorf("a clean run must still read as clean:\n%s", out)
	}
}

// captureOverlayReport runs fn with stdout captured, and fails the test if the
// report itself errored — these cases are all supposed to pass the lane.
func captureOverlayReport(t *testing.T, fn func() error) string {
	t.Helper()
	prev := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	reportErr := fn()
	_ = w.Close()
	os.Stdout = prev
	out := <-done
	if reportErr != nil {
		t.Fatalf("reportOverlay = %v, want nil (an absent object does not fail the lane)", reportErr)
	}
	return out
}

// A row the gate could not evaluate AT ALL — the field map naming a path the
// overlay does not declare — is REFUSED before any object is read, so it never
// counts as examined. The absent-object pass must not swallow it.
func TestARefusedRowIsNotDiscardedByTheAbsentObjectPass(t *testing.T) {
	refused := overlayVerdict{
		Field:  clusterspec.OverlayField{App: "loki", Value: []string{"gone"}},
		State:  stateRefused,
		Detail: "the overlay declares no loki.gone",
	}
	absent := overlayVerdict{
		Field: clusterspec.OverlayField{Kind: "statefulset", Namespace: "monitoring", Name: "loki-ingester"},
		State: stateObjectAbsent,
	}
	err := reportOverlay([]overlayVerdict{refused, absent}, nil, nil, 0)
	if err == nil {
		t.Fatal("a verdict this gate could not evaluate must not be discarded because another object " +
			"happened to be absent")
	}
	if !strings.Contains(err.Error(), "not evaluable") {
		t.Errorf("the failure must say what it could not do, got %v", err)
	}
}

// withOwnerExists stubs the Application-existence probe the absent-object branch
// reads.
func withOwnerExists(t *testing.T, exists bool) { withOwnerProbe(t, exists, true) }

func withOwnerProbe(t *testing.T, exists, answered bool) {
	t.Helper()
	prev := ownerExists
	ownerExists = func(clusterspec.OverlayField) (bool, bool) { return exists, answered }
	t.Cleanup(func() { ownerExists = prev })
}

// ABSENT HAS TWO CAUSES AND THEY ARE OPPOSITE. An object gone because a migration
// deleted it and Argo never put it back is the worst state this gate can be in —
// the workload running with no controller — and it looks exactly like an app the
// instance does not deploy. The owning Application is what tells them apart.
func TestAnAbsentObjectWhoseApplicationExistsFailsTheLane(t *testing.T) {
	withLiveObject(t, "", true, true)
	withDryRun(t, "", true)
	withOwnerExists(t, true)
	err := assertOverlayApplied()
	if err == nil {
		t.Fatal("an object its own Application declares, missing from the cluster, must fail — that is what " +
			"a recreate that never completed looks like")
	}
}

func TestAnAbsentObjectWithNoApplicationIsAnInstanceThatDoesNotRunIt(t *testing.T) {
	withLiveObject(t, "", true, true)
	withDryRun(t, "", true)
	withOwnerExists(t, false)
	out := captureOverlayReport(t, assertOverlayApplied)
	if !strings.Contains(out, "Nothing to check on this cluster") {
		t.Errorf("no object and no Application is an instance that does not run it:\n%s", out)
	}
}

// THE PROBE IS A READ, AND THE HANDLE IS WHAT MAKES THAT TRUE. The declaration
// justifies this lane's cluster-read grant by saying capability.Permits
// classifies `patch --dry-run=server` as a read; that is only worth anything if
// the call actually goes through a handle. Pinned by asking the handle directly:
// the argv this gate sends is permitted, and the same argv without the dry-run
// flag is refused.
func TestTheAppliabilityProbeIsPermittedAsAReadAndWouldBeRefusedWithoutTheDryRun(t *testing.T) {
	cluster := capability.MustCluster(Extension().MustBinding("overlay-applied"))
	argv := []string{"-n", "monitoring", "patch", "statefulset", "loki-ingester",
		"--dry-run=server", "-p", "{}"}
	if err := cluster.Permits(argv...); err != nil {
		t.Errorf("the gate's own probe is refused by the handle it runs on: %v", err)
	}
	var without []string
	for _, a := range argv {
		if a != "--dry-run=server" {
			without = append(without, a)
		}
	}
	if err := cluster.Permits(without...); err == nil {
		t.Error("a patch WITHOUT the dry-run flag must be refused — otherwise this lane's read-only " +
			"declaration rests on nothing but the argv happening to carry a flag")
	}
}

// AN UNANSWERABLE OWNER PROBE MUST NOT READ AS "not deployed here". Object gone
// plus one failed Application read used to make every row absent, which with a
// single mapped object is examined == 0, which is the vacuity pass — the gate
// green on the state this design calls its worst.
func TestAnUnreadableOwnerProbeFailsRatherThanPassingAsNotDeployed(t *testing.T) {
	withLiveObject(t, "", true, true) // the object is genuinely gone
	withDryRun(t, "", true)
	withOwnerProbe(t, false, false) // …and we could not tell whether its Application is
	if err := assertOverlayApplied(); err == nil {
		t.Fatal("could-not-tell about the owner of a missing object must fail: the other reading is a " +
			"recreate that never completed, and the workload running with no controller")
	}
}
