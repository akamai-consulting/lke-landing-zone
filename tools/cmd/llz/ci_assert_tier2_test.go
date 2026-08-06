package main

// Tests for the Tier-2 delivery gates: assert-log-ingestion, assert-eso-roundtrip,
// assert-alert-delivery and assert-grafana-dashboards. Each gate's judgement is a
// pure function over parsed input, so the whole verdict is exercised here without
// a cluster; the transport seams are replaced per test.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertobs"
	"sigs.k8s.io/yaml"
)

// ── assert-log-ingestion ─────────────────────────────────────────────────────

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
		for k, want := range assertobs.GrafanaSidecarLabels {
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

	for _, d := range assertobs.DefaultGrafanaDashboards {
		if !shipped[d] {
			t.Errorf("assertobs.DefaultGrafanaDashboards lists %q, which platform-apl does not ship — "+
				"the gate would fail on every cluster", d)
		}
	}
	for d := range shipped {
		if !containsString(assertobs.DefaultGrafanaDashboards, d) {
			t.Errorf("platform-apl ships dashboard %q that assertobs.DefaultGrafanaDashboards does not gate — "+
				"add it, or it can regress unnoticed", d)
		}
	}
}
