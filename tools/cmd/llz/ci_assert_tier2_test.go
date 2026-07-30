package main

// Tests for the Tier-2 delivery gates: assert-log-ingestion, assert-eso-roundtrip,
// assert-alert-delivery and assert-grafana-dashboards. Each gate's judgement is a
// pure function over parsed input, so the whole verdict is exercised here without
// a cluster; the transport seams are replaced per test.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// ── assert-log-ingestion ─────────────────────────────────────────────────────

func TestLogIngestionSelector(t *testing.T) {
	if got := logIngestionSelector("llz-reconciler"); got != `{namespace="llz-reconciler"}` {
		t.Errorf("unexpected selector %q", got)
	}
}

func TestEvalNamespaceIngestion(t *testing.T) {
	now := time.Now()
	streams := []lokiStream{{
		Labels:  map[string]string{"namespace": "llz-reconciler"},
		Entries: []lokiEntry{{At: now.Add(-time.Minute), Line: "reconcile ok"}},
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
	if v := evalNamespaceIngestion("x", []lokiStream{{Labels: map[string]string{}}}); v.FailWhy == "" {
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
	orig := withLoki
	t.Cleanup(func() { withLoki = orig })
	withLoki = func(_, _ string, fn func(func(string) ([]byte, error)) error) error {
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
	orig := withLoki
	t.Cleanup(func() { withLoki = orig })
	withLoki = func(_, _ string, _ func(func(string) ([]byte, error)) error) error {
		return errors.New("port-forward refused")
	}
	if err := runCIAssertLogIngestion("ns/svc:80", "t", []string{"a"}, 5, time.Hour, 0, time.Millisecond); err == nil {
		t.Error("an unreachable Loki must fail the gate")
	}
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

// ── assert-eso-roundtrip ─────────────────────────────────────────────────────

func TestStoreReady(t *testing.T) {
	ready := []byte(`{"status":{"conditions":[{"type":"Ready","status":"True","message":"valid"}]}}`)
	if ok, _, err := storeReady(ready); !ok || err != nil {
		t.Errorf("Ready=True must be ready, got (%v,%v)", ok, err)
	}
	notReady := []byte(`{"status":{"conditions":[{"type":"Ready","status":"False","message":"x509"}]}}`)
	if ok, msg, _ := storeReady(notReady); ok || !strings.Contains(msg, "x509") {
		t.Errorf("Ready=False must not be ready and must carry its message, got (%v,%q)", ok, msg)
	}
	// No condition is NOT ready — treating "no opinion" as healthy is how an
	// unauthenticated store passes.
	if ok, _, _ := storeReady([]byte(`{"status":{}}`)); ok {
		t.Error("a store with no Ready condition must not be treated as ready")
	}
	if _, _, err := storeReady([]byte(`nope`)); err == nil {
		t.Error("an unparseable store must be an error")
	}
}

func TestEvalExternalSecrets(t *testing.T) {
	now := time.Unix(1_720_000_000, 0).UTC()
	mk := func(refresh string, ready bool) []byte {
		status := "False"
		if ready {
			status = "True"
		}
		return []byte(`{"items":[{
		  "metadata":{"name":"creds","namespace":"llz-openbao"},
		  "spec":{"target":{"name":"creds-secret"}},
		  "status":{"refreshTime":"` + refresh + `","conditions":[{"type":"Ready","status":"` + status + `","reason":"SecretSynced"}]}
		}]}`)
	}
	have := map[string]bool{"llz-openbao/creds-secret": true}

	fresh, err := evalExternalSecrets(mk(now.Add(-5*time.Minute).Format(time.RFC3339), true), have, now, time.Hour)
	if err != nil || fresh[0].FailWhy != "" {
		t.Errorf("a recently-refreshed synced ES must pass: %+v (%v)", fresh, err)
	}

	// THE case this gate exists for: Ready, Secret present, but ESO stopped
	// re-reading. Every consumer still works; the value is frozen.
	stale, _ := evalExternalSecrets(mk(now.Add(-9*time.Hour).Format(time.RFC3339), true), have, now, time.Hour)
	if stale[0].FailWhy == "" {
		t.Error("a Ready ES with a stale refreshTime must fail — the Secret is serving a frozen value")
	}

	// Ready with a MISSING target Secret is a contradiction worth failing on.
	missing, _ := evalExternalSecrets(mk(now.Format(time.RFC3339), true), map[string]bool{}, now, time.Hour)
	if missing[0].FailWhy == "" {
		t.Error("a Ready ES whose target Secret is absent/empty must fail")
	}

	// No refreshTime at all is as blind as an old one.
	none, _ := evalExternalSecrets(mk("", true), have, now, time.Hour)
	if none[0].FailWhy == "" {
		t.Error("an ES with no refreshTime must fail closed")
	}

	notReady, _ := evalExternalSecrets(mk(now.Format(time.RFC3339), false), have, now, time.Hour)
	if notReady[0].FailWhy == "" {
		t.Error("a not-Ready ES must fail")
	}

	if _, err := evalExternalSecrets([]byte(`nope`), have, now, time.Hour); err == nil {
		t.Error("an unparseable list must be an error, not an empty set")
	}
}

func TestFilterByNamespace(t *testing.T) {
	vs := []esVerdict{{Name: "a/one"}, {Name: "b/two"}}
	if got := filterByNamespace(vs, nil); len(got) != 2 {
		t.Errorf("no filter must keep everything, got %d", len(got))
	}
	if got := filterByNamespace(vs, []string{"b"}); len(got) != 1 || got[0].Name != "b/two" {
		t.Errorf("unexpected filter result %+v", got)
	}
}

// seamESO points the ESO gate's three cluster reads at canned data.
func seamESO(t *testing.T, store, es []byte, secrets map[string]bool, storeErr, esErr error) {
	oS, oE, oSec := readClusterSecretStore, readExternalSecrets, readSecretsWithData
	t.Cleanup(func() { readClusterSecretStore, readExternalSecrets, readSecretsWithData = oS, oE, oSec })
	readClusterSecretStore = func(string) ([]byte, error) { return store, storeErr }
	readExternalSecrets = func([]string) ([]byte, error) { return es, esErr }
	readSecretsWithData = func() (map[string]bool, error) { return secrets, nil }
}

// A not-Ready store short-circuits: every ExternalSecret beneath it is serving a
// stale value, so reporting them individually would bury the actual cause.
func TestRunAssertESORoundTripFailsOnNotReadyStore(t *testing.T) {
	seamESO(t, []byte(`{"status":{"conditions":[{"type":"Ready","status":"False","message":"permission denied"}]}}`),
		[]byte(`{"items":[]}`), map[string]bool{}, nil, nil)
	err := runCIAssertESORoundTrip("openbao", nil, time.Hour, 0, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not Ready") {
		t.Errorf("a not-Ready store must fail with its own reason, got %v", err)
	}
}

// Zero ExternalSecrets must FAIL, not pass having examined nothing.
func TestRunAssertESORoundTripFailsOnEmptyInventory(t *testing.T) {
	seamESO(t, []byte(`{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`),
		[]byte(`{"items":[]}`), map[string]bool{}, nil, nil)
	if err := runCIAssertESORoundTrip("openbao", nil, time.Hour, 0, time.Millisecond); err == nil {
		t.Error("finding zero ExternalSecrets must fail rather than pass vacuously")
	}
}

// ── assert-alert-delivery ────────────────────────────────────────────────────

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
	orig := withPrometheus
	t.Cleanup(func() { withPrometheus = orig })
	withPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
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

func dashboardCM(labels map[string]string, data string) []byte {
	obj := map[string]any{
		"metadata": map[string]any{"labels": labels},
		"data":     map[string]string{"dash.json": data},
	}
	b, _ := json.Marshal(obj)
	return b
}

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
func TestDefaultGrafanaDashboardsMatchTheManifests(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "platform-apl", "components", "observability")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("observability manifests not reachable from the test cwd: %v", err)
	}

	shipped := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "dashboard.yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		var cm struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string            `json:"name"`
				Namespace string            `json:"namespace"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(raw, &cm); err != nil || cm.Kind != "ConfigMap" {
			continue
		}
		shipped[cm.Metadata.Namespace+"/"+cm.Metadata.Name] = true

		// While we are here: the manifest itself must carry both sidecar labels.
		// Catching this at PR time beats catching it in an e2e cycle.
		for k, want := range grafanaSidecarLabels {
			if cm.Metadata.Labels[k] != want {
				t.Errorf("%s: manifest is missing sidecar label %s=%q (found %q) — "+
					"it would render on one stack and vanish on the other",
					e.Name(), k, want, cm.Metadata.Labels[k])
			}
		}
	}
	if len(shipped) == 0 {
		t.Fatal("found no dashboard ConfigMaps — this guard would pass vacuously")
	}

	for _, d := range defaultGrafanaDashboards {
		if !shipped[d] {
			t.Errorf("defaultGrafanaDashboards lists %q, which platform-apl does not ship — "+
				"the gate would fail on every cluster", d)
		}
	}
	for d := range shipped {
		if !containsString(defaultGrafanaDashboards, d) {
			t.Errorf("platform-apl ships dashboard %q that defaultGrafanaDashboards does not gate — "+
				"add it, or it can regress unnoticed", d)
		}
	}
}
