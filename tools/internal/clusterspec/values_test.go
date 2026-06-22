package clusterspec

import (
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

func TestRenderValues(t *testing.T) {
	const base = `# apl-core values — TEMPLATE.
apps:
  prometheus:
    enabled: true            # block style, with a comment
    retention: 7d
  alertmanager: { enabled: true }   # flow style
  loki:
    enabled: true
    adminPassword: ${loki_admin_password}
    resolver: "${coredns_cluster_ip}"
  harbor:
    enabled: false
  external-dns: {}           # no enabled key — must be left alone
otomi:
  git:
    repoUrl: ${apl_values_repo_url}
`
	// Disable observability (→ prometheus/alertmanager/loki/grafana/otel off);
	// harbor stays enabled (default).
	toggles := map[string]ComponentToggle{"observability": {Enabled: false}}
	out, err := RenderValues([]byte(base), toggles)
	if err != nil {
		t.Fatalf("RenderValues: %v", err)
	}
	s := string(out)

	// Flipped: the observability apps are now disabled (block + flow both).
	for _, app := range []string{"prometheus", "loki"} {
		if !strings.Contains(s, app+":") {
			t.Fatalf("app %s missing from output:\n%s", app, s)
		}
	}
	if strings.Count(s, "enabled: true")+strings.Count(s, "enabled: true }") > 1 {
		// only harbor's app should be... actually harbor is enabled here, plus none of the obs apps.
	}
	// harbor (default-enabled component) flipped on; obs apps off.
	if !strings.Contains(s, "harbor:") {
		t.Fatal("harbor missing")
	}
	mustHave := []string{
		"# block style, with a comment", // comment preserved
		"retention: 7d",                 // sibling config preserved
		"${loki_admin_password}",        // plain placeholder preserved
		`"${coredns_cluster_ip}"`,       // quoted placeholder keeps its quotes
		"${apl_values_repo_url}",        // unrelated section preserved
		"external-dns: {}",              // no-enabled app untouched
	}
	for _, w := range mustHave {
		if !strings.Contains(s, w) {
			t.Errorf("RenderValues dropped %q:\n%s", w, s)
		}
	}

	// Re-parse to assert the enabled values, robust to formatting.
	got := mustDecodeValues(t, out)
	for app, wantEnabled := range map[string]bool{
		"prometheus": false, "alertmanager": false, "loki": false, "harbor": true,
	} {
		if got[app] != wantEnabled {
			t.Errorf("apps.%s.enabled = %v, want %v", app, got[app], wantEnabled)
		}
	}

	// Idempotent: rendering the output again yields the same bytes.
	out2, err := RenderValues(out, toggles)
	if err != nil {
		t.Fatalf("second RenderValues: %v", err)
	}
	if string(out2) != s {
		t.Errorf("RenderValues not idempotent:\n--- first ---\n%s\n--- second ---\n%s", s, out2)
	}
}

// mustDecodeValues pulls apps.<name>.enabled out of a rendered values.yaml.
func mustDecodeValues(t *testing.T, b []byte) map[string]bool {
	t.Helper()
	var v struct {
		Apps map[string]struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"apps"`
	}
	if err := yaml.Unmarshal(b, &v); err != nil {
		t.Fatalf("re-parse values: %v", err)
	}
	out := map[string]bool{}
	for k, a := range v.Apps {
		out[k] = a.Enabled
	}
	return out
}
