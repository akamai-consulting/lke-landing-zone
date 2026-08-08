package assertobs

// ELEVEN OF TWENTY TESTS FROM ci_assert_tier2_test.go.
//
// Another file named for a coverage TIER, and the split is now routine enough to
// be mechanical: classify each function by whether it references a symbol this
// package defines, then move by LINE RANGE off the parsed `^func Name(`
// boundaries. The nine that stayed test the ESO round-trip and the dashboard
// manifest set, which are package main's.
//
// Twelfth, thirteenth and fourteenth stranded tests found this way. The two
// naming patterns — a coverage METRIC, or the COMMAND that happens to call the
// code — still account for every one.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLogIngestionSelector(t *testing.T) {
	if got := logIngestionSelector("llz-reconciler"); got != `{namespace="llz-reconciler"}` {
		t.Errorf("unexpected selector %q", got)
	}
}

func TestEvalNamespaceIngestion(t *testing.T) {
	now := time.Now()
	streams := []LokiStream{{
		Labels:  map[string]string{"namespace": "llz-reconciler"},
		Entries: []LokiEntry{{At: now.Add(-time.Minute), Line: "reconcile ok"}},
	}}
	if v := evalNamespaceIngestion("llz-reconciler", streams); v.FailWhy != "" || v.Entries != 1 {
		t.Errorf("a namespace with recent lines must pass: %+v", v)
	}
	// Zero entries is the whole point — it must FAIL, not pass with a note.
	v := evalNamespaceIngestion("llz-openbao", nil)
	if v.FailWhy == "" {
		t.Fatal("a namespace with no log lines must fail — that is the collector regression")
	}
	if !strings.Contains(v.FailWhy, "relabel") {
		t.Errorf("the failure should point at the collector's discovery/relabel path, got %q", v.FailWhy)
	}
	// A stream that exists but carried nothing is equally a failure: a dropped
	// `namespace` label leaves the stream findable and empty.
	if v := evalNamespaceIngestion("x", []LokiStream{{Labels: map[string]string{}}}); v.FailWhy == "" {
		t.Error("an empty stream must fail")
	}
}

func TestRunAssertLogIngestionRefusesVacuousArguments(t *testing.T) {
	if err := runCIAssertLogIngestion("ns/svc:80", "t", nil, 5, time.Hour, 0, time.Millisecond); err == nil {
		t.Error("no namespaces must fail rather than pass having checked nothing")
	}
	if err := runCIAssertLogIngestion("ns/svc:80", "t", []string{"a"}, 5, 0, 0, time.Millisecond); err == nil {
		t.Error("a zero lookback can never contain a line and must be rejected")
	}
}

func TestRunAssertLogIngestionFailsOnSilentNamespace(t *testing.T) {
	now := time.Now().UnixNano()
	body := func(lines int) []byte {
		if lines == 0 {
			return []byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`)
		}
		return []byte(`{"status":"success","data":{"resultType":"streams","result":[
		  {"stream":{"namespace":"llz-reconciler"},"values":[["` +
			jsonInt(now) + `","reconcile ok"]]}]}}`)
	}
	orig := WithLoki
	t.Cleanup(func() { WithLoki = orig })
	WithLoki = func(_, _ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(path string) ([]byte, error) {
			if strings.Contains(path, "llz-openbao") {
				return body(0), nil // silent namespace
			}
			return body(1), nil
		})
	}
	err := runCIAssertLogIngestion("ns/svc:80", "platform",
		[]string{"llz-reconciler", "llz-openbao"}, 5, 30*time.Minute, 0, time.Millisecond)
	if err == nil {
		t.Fatal("a namespace shipping no logs must fail the gate")
	}
	if !strings.Contains(err.Error(), "llz-openbao") {
		t.Errorf("the failure must name the silent namespace, got %v", err)
	}
}

// An unreachable Loki must FAIL, never be read as "the collector stopped".
func TestRunAssertLogIngestionFailsOnUnreachableLoki(t *testing.T) {
	orig := WithLoki
	t.Cleanup(func() { WithLoki = orig })
	WithLoki = func(_, _ string, _ func(func(string) ([]byte, error)) error) error {
		return errors.New("port-forward refused")
	}
	if err := runCIAssertLogIngestion("ns/svc:80", "t", []string{"a"}, 5, time.Hour, 0, time.Millisecond); err == nil {
		t.Error("an unreachable Loki must fail the gate")
	}
}

func TestActiveAlertmanagers(t *testing.T) {
	raw := []byte(`{"status":"success","data":{
	  "activeAlertmanagers":[{"url":"http://10.0.0.1:9093/api/v2/alerts"}],
	  "droppedAlertmanagers":[{"url":"http://10.0.0.2:9093/api/v2/alerts"}]}}`)
	active, dropped, err := activeAlertmanagers(raw)
	if err != nil || len(active) != 1 || dropped != 1 {
		t.Errorf("unexpected parse: active=%v dropped=%d err=%v", active, dropped, err)
	}
	// Zero active is the silent void this gate exists for.
	none := []byte(`{"status":"success","data":{"activeAlertmanagers":[],"droppedAlertmanagers":[]}}`)
	if a, _, err := activeAlertmanagers(none); err != nil || len(a) != 0 {
		t.Errorf("empty must parse cleanly as zero active, got %v/%v", a, err)
	}
	if _, _, err := activeAlertmanagers([]byte(`{"status":"error","error":"boom"}`)); err == nil {
		t.Error("a Prometheus error must not decode as zero alertmanagers")
	}
}

func TestAlertmanagerReady(t *testing.T) {
	ok := []byte(`{"cluster":{"status":"ready"},"versionInfo":{"version":"0.27.0"}}`)
	desc, err := alertmanagerReady(ok)
	if err != nil || !strings.Contains(desc, "0.27.0") {
		t.Errorf("unexpected (%q,%v)", desc, err)
	}
	// A 200 from something that is not Alertmanager must not count — the gate is
	// distinguishing "something answered" from "the router answered".
	if _, err := alertmanagerReady([]byte(`{"hello":"world"}`)); err == nil {
		t.Error("a body with neither cluster nor versionInfo must not read as a live Alertmanager")
	}
	if _, err := alertmanagerReady([]byte(`nope`)); err == nil {
		t.Error("an unparseable status must be an error")
	}
}

func TestRunAssertAlertDeliveryFailsWithNoAlertmanager(t *testing.T) {
	orig := WithPrometheus
	t.Cleanup(func() { WithPrometheus = orig })
	WithPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(string) ([]byte, error) {
			return []byte(`{"status":"success","data":{"activeAlertmanagers":[],"droppedAlertmanagers":[]}}`), nil
		})
	}
	err := runCIAssertAlertDelivery("ns/prom:9090", "ns/am:9093", 0, time.Millisecond)
	if err == nil {
		t.Fatal("no active Alertmanager must fail — firing alerts would go nowhere")
	}
	if !strings.Contains(err.Error(), "nowhere") {
		t.Errorf("the failure should say alerts go nowhere, got %v", err)
	}
}

// ── assert-grafana-dashboards ────────────────────────────────────────────────

func TestEvalDashboardConfigMap(t *testing.T) {
	good := map[string]string{"grafana_dashboard": "1", "release": "grafana-dashboards"}
	if v := evalDashboardConfigMap("ns/d", dashboardCM(good, `{"title":"x","panels":[{},{}]}`)); v.FailWhy != "" || v.Panels != 2 {
		t.Errorf("a well-labelled dashboard must pass: %+v", v)
	}

	// Only ONE sidecar label: renders on the stack you tested, vanishes on the
	// other. This is the cross-stack bug no single-cluster test can see.
	onlySelfInstall := map[string]string{"grafana_dashboard": "1"}
	v := evalDashboardConfigMap("ns/d", dashboardCM(onlySelfInstall, `{"panels":[]}`))
	if v.FailWhy == "" {
		t.Fatal("a dashboard labelled for only one sidecar must fail")
	}
	if !strings.Contains(v.FailWhy, "release") {
		t.Errorf("the failure must name the missing label, got %q", v.FailWhy)
	}

	// Presence is not the invariant — the VALUE matters.
	wrongValue := map[string]string{"grafana_dashboard": "false", "release": "grafana-dashboards"}
	if v := evalDashboardConfigMap("ns/d", dashboardCM(wrongValue, `{"panels":[]}`)); v.FailWhy == "" {
		t.Error(`grafana_dashboard: "false" must fail — checking for the key's presence would pass it`)
	}

	// Malformed dashboard JSON: the sidecar logs and moves on, which looks
	// exactly like success.
	if v := evalDashboardConfigMap("ns/d", dashboardCM(good, `{not json`)); v.FailWhy == "" {
		t.Error("unparseable dashboard JSON must fail")
	}
	if v := evalDashboardConfigMap("ns/d", []byte(`{"metadata":{"labels":{"grafana_dashboard":"1","release":"grafana-dashboards"}},"data":{}}`)); v.FailWhy == "" {
		t.Error("a dashboard ConfigMap with no data must fail")
	}
}

func TestProbeGrafanaDashboardsMissingConfigMapFails(t *testing.T) {
	orig := readDashboardConfigMap
	t.Cleanup(func() { readDashboardConfigMap = orig })
	readDashboardConfigMap = func(string, string) ([]byte, error) { return nil, errors.New("NotFound") }
	vs := probeGrafanaDashboards([]string{"llz-observability/llz-day2-dashboard"})
	if len(failedDashboards(vs)) != 1 {
		t.Errorf("a missing dashboard ConfigMap must fail, got %+v", vs)
	}
	// A malformed coordinate must fail rather than be skipped.
	if vs := probeGrafanaDashboards([]string{"no-slash"}); len(failedDashboards(vs)) != 1 {
		t.Error("a dashboard not in namespace/name form must fail")
	}
}

func TestRunAssertGrafanaDashboardsRefusesEmptyList(t *testing.T) {
	if err := runCIAssertGrafanaDashboards(nil, 0, time.Millisecond); err == nil {
		t.Error("an empty dashboard list must fail rather than pass having checked nothing")
	}
}

// TestDefaultGrafanaDashboardsMatchTheManifests is the coupling guard between
// this gate's expected set and the ConfigMaps platform-apl actually ships. A
// dashboard renamed in the manifest (or a new one added) silently drops out of
// the gate otherwise — and the first default written here was wrong by exactly
// that kind of guess.
func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

// ── assert-eso-roundtrip ─────────────────────────────────────────────────────

func dashboardCM(labels map[string]string, data string) []byte {
	obj := map[string]any{
		"metadata": map[string]any{"labels": labels},
		"data":     map[string]string{"dash.json": data},
	}
	b, _ := json.Marshal(obj)
	return b
}
