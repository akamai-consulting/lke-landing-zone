package clusterspec

import (
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// digStr walks a nested map[string]any decoded from YAML and returns the string
// at the key path (or "", false).
func digStr(t *testing.T, y string, keys ...string) (string, bool) {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(y), &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, y)
	}
	var cur any = m
	for _, k := range keys {
		cm, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = cm[k]
		if !ok {
			return "", false
		}
	}
	s, ok := cur.(string)
	return s, ok
}

func TestRenderObjOverlayShared(t *testing.T) {
	y := RenderObjOverlayShared()
	if got, _ := digStr(t, y, "obj", "provider", "type"); got != "linode" {
		t.Errorf("provider.type = %q, want linode", got)
	}
	if got, _ := digStr(t, y, "obj", "provider", "linode", "accessKeyId"); got != ObjAccessKeyIDPlaceholder {
		t.Errorf("accessKeyId = %q, want placeholder %q", got, ObjAccessKeyIDPlaceholder)
	}
	if got, _ := digStr(t, y, "obj", "provider", "linode", "secretAccessKey"); got != ObjSecretAccessKeyPlaceholder {
		t.Errorf("secretAccessKey = %q, want placeholder", got)
	}
	// showWizard must be the literal boolean false, not the string "false".
	if !strings.Contains(y, "showWizard: false") {
		t.Errorf("want `showWizard: false`, got:\n%s", y)
	}
	// The shared base carries no region/buckets (those are per-env).
	if strings.Contains(y, "region:") || strings.Contains(y, "buckets:") {
		t.Errorf("shared obj overlay must not carry region/buckets:\n%s", y)
	}
}

func TestRenderObjOverlayEnv(t *testing.T) {
	y := RenderObjOverlayEnv("primary", "us-ord-1")
	if got, _ := digStr(t, y, "obj", "provider", "linode", "region"); got != "us-ord-1" {
		t.Errorf("region = %q, want us-ord-1", got)
	}
	if got, _ := digStr(t, y, "obj", "provider", "linode", "buckets", "loki"); got != "platform-loki-chunks-primary" {
		t.Errorf("buckets.loki = %q, want platform-loki-chunks-primary", got)
	}
	if got, _ := digStr(t, y, "obj", "provider", "linode", "buckets", "harbor"); got != "platform-harbor-registry-primary" {
		t.Errorf("buckets.harbor = %q, want platform-harbor-registry-primary", got)
	}
	// No credential fields in the per-env override (they live in _shared).
	if strings.Contains(y, "accessKeyId") || strings.Contains(y, "secretAccessKey") {
		t.Errorf("env obj overlay must not carry credentials:\n%s", y)
	}
	if RenderObjOverlayEnv("primary", "") != "" {
		t.Error("no object-storage cluster → empty env obj overlay")
	}
}

func TestRenderAppsOverlayShared(t *testing.T) {
	y := RenderAppsOverlayShared()
	var doc appsOverlayDoc
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, a := range aplStaticDisabledApps {
		tog, ok := doc.Apps[a]
		if !ok {
			t.Errorf("static app %q missing from shared apps overlay", a)
			continue
		}
		if tog.Enabled {
			t.Errorf("static app %q must be enabled:false", a)
		}
	}
}

func TestRenderAppsOverlayEnv(t *testing.T) {
	// ComponentEnabled(toggles, name) == present-in-map && !toggle.DefaultDisabled,
	// so a component present with a zero toggle is enabled.
	on := RenderAppsOverlayEnv(map[string]ComponentToggle{"observability": {}, "harbor": {}})
	var doc appsOverlayDoc
	if err := yaml.Unmarshal([]byte(on), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, app := range []string{"prometheus", "loki", "grafana", "otel", "alertmanager", "harbor"} {
		if tog, ok := doc.Apps[app]; !ok || !tog.Enabled {
			t.Errorf("app %q: want enabled:true (present component), got ok=%v enabled=%v", app, ok, tog.Enabled)
		}
	}
	// Explicitly disabling observability (tri-state Enabled=false) flips its
	// apl-core apps off — the toggle overrides the component's default-on.
	off := RenderAppsOverlayEnv(map[string]ComponentToggle{"observability": {Enabled: boolPtr(false)}})
	var offDoc appsOverlayDoc
	if err := yaml.Unmarshal([]byte(off), &offDoc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tog := offDoc.Apps["prometheus"]; tog.Enabled {
		t.Errorf("prometheus with observability explicitly disabled must be enabled:false")
	}
}

func TestMergeOverlay(t *testing.T) {
	merged, err := MergeOverlay([]byte(RenderObjOverlayShared()), []byte(RenderObjOverlayEnv("primary", "us-ord-1")))
	if err != nil {
		t.Fatalf("MergeOverlay: %v", err)
	}
	y := string(merged)
	// The union: shared's credentials/type + env's region/buckets in one doc.
	if got, _ := digStr(t, y, "obj", "provider", "type"); got != "linode" {
		t.Errorf("merged provider.type = %q, want linode", got)
	}
	if got, _ := digStr(t, y, "obj", "provider", "linode", "accessKeyId"); got != ObjAccessKeyIDPlaceholder {
		t.Errorf("merged accessKeyId = %q, want placeholder", got)
	}
	if got, _ := digStr(t, y, "obj", "provider", "linode", "region"); got != "us-ord-1" {
		t.Errorf("merged region = %q, want us-ord-1", got)
	}
	if got, _ := digStr(t, y, "obj", "provider", "linode", "buckets", "loki"); got != "platform-loki-chunks-primary" {
		t.Errorf("merged buckets.loki = %q", got)
	}
}

func TestMergeOverlay_EnvWinsScalar(t *testing.T) {
	shared := []byte("obj:\n  provider:\n    linode:\n      region: shared-region\n")
	env := []byte("obj:\n  provider:\n    linode:\n      region: env-region\n")
	merged, err := MergeOverlay(shared, env)
	if err != nil {
		t.Fatalf("MergeOverlay: %v", err)
	}
	if got, _ := digStr(t, string(merged), "obj", "provider", "linode", "region"); got != "env-region" {
		t.Errorf("env must win scalar conflict: got %q", got)
	}
}

func TestMergeOverlay_EmptyLayers(t *testing.T) {
	merged, err := MergeOverlay(nil, []byte(RenderObjOverlayEnv("primary", "us-ord-1")))
	if err != nil {
		t.Fatalf("MergeOverlay(nil, env): %v", err)
	}
	if got, _ := digStr(t, string(merged), "obj", "provider", "linode", "region"); got != "us-ord-1" {
		t.Errorf("nil shared base must pass env through: got %q", got)
	}
	if _, err := MergeOverlay(nil, nil); err != nil {
		t.Errorf("MergeOverlay(nil, nil) should be a clean empty merge: %v", err)
	}
}

func TestFillObjPlaceholders(t *testing.T) {
	merged, _ := MergeOverlay([]byte(RenderObjOverlayShared()), []byte(RenderObjOverlayEnv("primary", "us-ord-1")))
	filled := FillObjPlaceholders(merged, "AKID123", "SEKRET456")
	y := string(filled)
	if strings.Contains(y, ObjAccessKeyIDPlaceholder) || strings.Contains(y, ObjSecretAccessKeyPlaceholder) {
		t.Errorf("placeholders must be gone after fill:\n%s", y)
	}
	if got, _ := digStr(t, y, "obj", "provider", "linode", "accessKeyId"); got != "AKID123" {
		t.Errorf("accessKeyId = %q, want AKID123", got)
	}
	if got, _ := digStr(t, y, "obj", "provider", "linode", "secretAccessKey"); got != "SEKRET456" {
		t.Errorf("secretAccessKey = %q, want SEKRET456", got)
	}
	// An empty read must NOT blank the credential — the placeholder stays.
	partial := FillObjPlaceholders(merged, "AKID123", "")
	if !strings.Contains(string(partial), ObjSecretAccessKeyPlaceholder) {
		t.Error("empty secret must leave the placeholder, not an empty value")
	}
}
