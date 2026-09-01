package reconcilelanes

// overlay_delivery_test.go — what the gauges say, and (more important) when they
// say nothing.
//
// The three silences are the substance: an unreadable apiserver, an object that
// does not exist here, and a row that no longer resolves. Each would, if
// published as a 0, page someone about a cluster that is fine; each, if published
// as a 1, is the silent green this whole family of checks exists to end.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
)

const (
	overlayBrownfieldSTS = `{"apiVersion":"apps/v1","kind":"StatefulSet",
      "metadata":{"name":"loki-ingester","namespace":"monitoring"},
      "spec":{"template":{"spec":{"containers":[{"name":"ingester",
        "resources":{"limits":{"cpu":"1","memory":"1Gi"}}}]}}}}`

	overlayDeliveredSTS = `{"apiVersion":"apps/v1","kind":"StatefulSet",
      "metadata":{"name":"loki-ingester","namespace":"monitoring"},
      "spec":{"template":{"spec":{"containers":[{"name":"ingester","resources":{
        "limits":{"cpu":"1","memory":"3Gi"},"requests":{"cpu":"100m","memory":"512Mi"}}}]}},
        "volumeClaimTemplates":[{"metadata":{"name":"data"}}]}}`
)

// stubKube answers every GET with one canned object, status and error.
type stubKube struct {
	capability.KubeAPI
	obj    string
	status int
	err    error
	paths  []string
}

func (s *stubKube) GetJSON(_ context.Context, path string) (map[string]any, int, error) {
	s.paths = append(s.paths, path)
	if s.err != nil {
		return nil, 0, s.err
	}
	if s.obj == "" {
		return nil, s.status, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s.obj), &m); err != nil {
		return nil, 0, err
	}
	return m, s.status, nil
}

// hasSample reports whether a metric family actually carries a SAMPLE line, as
// opposed to the `# HELP`/`# TYPE` header a declared-but-empty family still
// prints. The distinction is the invariant: an empty family means "not published
// this pass", which is what makes a stale value impossible.
func hasSample(out, metric string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, metric+"{") || strings.HasPrefix(line, metric+" ") {
			return true
		}
	}
	return false
}

func sample(t *testing.T, k *stubKube) (string, error) {
	t.Helper()
	reg := metrics.NewRegistry()
	err := SampleOverlayDelivery(context.Background(), k, reg)
	var b strings.Builder
	reg.WriteTo(&b)
	return b.String(), err
}

func TestABrownfieldObjectPublishesUndeliveredAndAPendingMigration(t *testing.T) {
	out, err := sample(t, &stubKube{obj: overlayBrownfieldSTS, status: 200})
	if err != nil {
		t.Fatalf("a readable object is not an error: %v", err)
	}
	if !strings.Contains(out, `llz_overlay_field_delivered{app="loki",object="statefulset/loki-ingester",path="loki.ingester.resources.limits.memory"} 0`) {
		t.Errorf("the undelivered memory limit must publish 0:\n%s", out)
	}
	if !strings.Contains(out, `llz_brownfield_migration_pending{id="`+clusterspec.LokiWALPVCMigration+`"`) ||
		!strings.Contains(out, `} 1`) {
		t.Errorf("a StatefulSet with no claim templates must publish the migration as pending:\n%s", out)
	}
}

func TestAConvergedObjectPublishesDeliveredAndNoPendingMigration(t *testing.T) {
	out, err := sample(t, &stubKube{obj: overlayDeliveredSTS, status: 200})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, `llz_overlay_field_delivered{app="loki",object="statefulset/loki-ingester",path="loki.ingester.resources.limits.memory"} 0`) {
		t.Errorf("a 3Gi limit is delivered:\n%s", out)
	}
	if !strings.Contains(out, `llz_brownfield_migration_pending{id="`+clusterspec.LokiWALPVCMigration+`",path="loki.ingester.persistence.enabled"} 0`) {
		t.Errorf("an object carrying the claim template has no migration pending:\n%s", out)
	}
}

// ── the three silences ───────────────────────────────────────────────────────

func TestAnUnreachableApiserverPublishesNothingAndReportsTheFailure(t *testing.T) {
	out, err := sample(t, &stubKube{err: context.DeadlineExceeded})
	if err == nil {
		t.Fatal("a failed read must surface as the pass failing (llz_reconcile_up), not as silence alone")
	}
	if hasSample(out, "llz_overlay_field_delivered") {
		t.Errorf("no series may be published for a cluster that did not answer:\n%s", out)
	}
}

// A cluster that does not run Loki is not a cluster with an undelivered Loki
// value. It has no series at all.
func TestAnObjectThatDoesNotExistHerePublishesNothing(t *testing.T) {
	out, err := sample(t, &stubKube{status: 404})
	if err != nil {
		t.Fatalf("a 404 is a legitimate answer, not a failure: %v", err)
	}
	if hasSample(out, "llz_overlay_field_delivered") {
		t.Errorf("an absent object must not publish a delivery verdict:\n%s", out)
	}
}

// The chart renamed the container the row points at. That is a stale table, and
// publishing 0 would blame the cluster for it.
func TestARowThatNoLongerResolvesPublishesNothingForThatField(t *testing.T) {
	renamed := strings.Replace(overlayDeliveredSTS, `"name":"ingester"`, `"name":"loki"`, 1)
	out, err := sample(t, &stubKube{obj: renamed, status: 200})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, `path="loki.ingester.resources.limits.memory"`) {
		t.Errorf("a row that does not resolve must publish no verdict for its field:\n%s", out)
	}
	// The claim-template row still resolves and must still be published — one stale
	// row does not blind the rest.
	if !hasSample(out, "llz_brownfield_migration_pending") {
		t.Errorf("the rows that DO resolve must still publish:\n%s", out)
	}
}

// The lane reads through the API path, not through kubectl. If the two spellings
// of the object ever disagree this is where it shows up as a 404.
func TestTheLaneReadsTheApiPathTheFieldMapDeclares(t *testing.T) {
	k := &stubKube{obj: overlayDeliveredSTS, status: 200}
	if _, err := sample(t, k); err != nil {
		t.Fatal(err)
	}
	want := "/apis/apps/v1/namespaces/monitoring/statefulsets/loki-ingester"
	for _, p := range k.paths {
		if p != want {
			t.Errorf("read %q, want %q", p, want)
		}
	}
	if len(k.paths) == 0 {
		t.Fatal("the lane read nothing — it would publish an all-clear having asked no question")
	}
}

// A SERIES THAT STOPS BEING PUBLISHED MUST DISAPPEAR. In a long-lived reconciler
// SetGauge has no delete, so an object that goes away — or a row that stops
// resolving — would go on serving its last value forever, reporting "delivered"
// about something nobody can see any more. SetGaugeFamily replaces the family
// each pass; this is the test that says so.
func TestASeriesStopsBeingPublishedWhenTheObjectGoesAway(t *testing.T) {
	reg := metrics.NewRegistry()
	present := &stubKube{obj: overlayDeliveredSTS, status: 200}
	if err := SampleOverlayDelivery(context.Background(), present, reg); err != nil {
		t.Fatal(err)
	}
	var first strings.Builder
	reg.WriteTo(&first)
	if !hasSample(first.String(), "llz_overlay_field_delivered") {
		t.Fatalf("the first pass published nothing, so this test cannot show a stale one:\n%s", first.String())
	}

	// Same registry, next pass: the object is gone.
	gone := &stubKube{status: 404}
	if err := SampleOverlayDelivery(context.Background(), gone, reg); err != nil {
		t.Fatal(err)
	}
	var second strings.Builder
	reg.WriteTo(&second)
	if hasSample(second.String(), "llz_overlay_field_delivered") {
		t.Errorf("the series survived the object it describes — an alert on it would now be reading a "+
			"value from a cluster state that no longer exists:\n%s", second.String())
	}
}
