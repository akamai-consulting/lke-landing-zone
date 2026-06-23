package clusterspec

import (
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

func TestRenderValues(t *testing.T) {
	const base = `# apl-core values — TEMPLATE.
cluster:
  name: ${cluster_name}
  provider: linode
  domainSuffix: ${cluster_domain}
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
  hasExternalDNS: true
  hasExternalIDP: false
  git:
    repoUrl: ${apl_values_repo_url}
dns:
  domainFilters:
    - ${cluster_domain}
`
	// Disable observability (→ prometheus/alertmanager/loki/grafana/otel off);
	// harbor stays enabled (default). Identity + platform come from the spec.
	toggles := map[string]ComponentToggle{"observability": {Enabled: boolPtr(false)}}
	id := ValuesIdentity{
		ClusterName:  "acme-prod",
		DomainSuffix: "prod.acme.internal",
		ExternalDNS:  true,
		ExternalIDP:  true, // overrides the base literal (false) — spec wins
	}
	out, err := RenderValues([]byte(base), toggles, id)
	if err != nil {
		t.Fatalf("RenderValues: %v", err)
	}
	s := string(out)

	// Spec-owned identity + platform written straight in (templatefile hop cut).
	for _, w := range []string{
		"name: acme-prod",
		"domainSuffix: prod.acme.internal",
		"- prod.acme.internal", // dns.domainFilters[0]
		"hasExternalDNS: true",
		"hasExternalIDP: true", // base said false; spec override took
	} {
		if !strings.Contains(s, w) {
			t.Errorf("identity/platform not rendered: missing %q:\n%s", w, s)
		}
	}
	for _, ph := range []string{"${cluster_name}", "${cluster_domain}"} {
		if strings.Contains(s, ph) {
			t.Errorf("spec-owned placeholder %q should be resolved, still present:\n%s", ph, s)
		}
	}

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
	out2, err := RenderValues(out, toggles, id)
	if err != nil {
		t.Fatalf("second RenderValues: %v", err)
	}
	if string(out2) != s {
		t.Errorf("RenderValues not idempotent:\n--- first ---\n%s\n--- second ---\n%s", s, out2)
	}
}

func TestRenderValues_Sizing(t *testing.T) {
	const base = `apps:
  prometheus:
    enabled: true
    retention: 7d
    storageSize: 10Gi
    replicas: 1
  harbor:
    enabled: true
    _rawValues:
      persistence:
        persistentVolumeClaim:
          registry: { size: 20Gi }
`
	toggles := map[string]ComponentToggle{
		"observability": {Enabled: boolPtr(true), Retention: "30d", Storage: "50Gi", Replicas: intPtr(2)},
		"harbor":        {Enabled: boolPtr(true), RegistryStorage: "100Gi"},
	}
	out, err := RenderValues([]byte(base), toggles, ValuesIdentity{})
	if err != nil {
		t.Fatalf("RenderValues: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"retention: 30d", // observability.retention → prometheus.retention
		"storageSize: 50Gi",
		"replicas: 2",
		"size: 100Gi", // harbor.registryStorage → registry PVC
	} {
		if !strings.Contains(s, want) {
			t.Errorf("sizing not rendered: missing %q:\n%s", want, s)
		}
	}
	// Unset knobs leave the base default (no observability storage→loki spillover etc.).
	out2, err := RenderValues([]byte(base), map[string]ComponentToggle{"observability": {Enabled: boolPtr(true)}}, ValuesIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out2), "retention: 7d") {
		t.Errorf("unset retention should keep base default 7d:\n%s", out2)
	}
}

func intPtr(i int) *int { return &i }

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
