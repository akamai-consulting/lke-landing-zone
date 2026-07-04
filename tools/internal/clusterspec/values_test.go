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

// Regression: gitea is DefaultDisabled on v6, so a default render (no explicit
// gitea toggle) must NOT re-enable it — even if the base template somehow carries
// enabled: true. Guards the bug where the gitea component lacked DefaultDisabled
// and RenderValues flipped the committed `gitea: { enabled: false }` back to true.
func TestRenderValues_GiteaDisabledByDefault(t *testing.T) {
	const base = `apps:
  gitea:
    enabled: true
  harbor:
    enabled: false
`
	// No gitea toggle → its DefaultDisabled default (off) must win.
	out, err := RenderValues([]byte(base), map[string]ComponentToggle{}, ValuesIdentity{})
	if err != nil {
		t.Fatalf("RenderValues: %v", err)
	}
	got := mustDecodeValues(t, out)
	if got["gitea"] {
		t.Errorf("gitea re-enabled by default render; want disabled:\n%s", out)
	}
	// harbor (default-enabled) still flips on — proves the render did run.
	if !got["harbor"] {
		t.Errorf("harbor should be enabled by default:\n%s", out)
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

func TestObjectStoreWiring(t *testing.T) {
	chunks, ruler, admin, lokiEndpoint, region, harborBucket, harborEndpoint :=
		objectStoreWiring("primary", "us-ord-1")
	for got, want := range map[string]string{
		chunks:         "platform-loki-chunks-primary",
		ruler:          "platform-loki-ruler-primary",
		admin:          "platform-loki-admin-primary",
		lokiEndpoint:   "us-ord-1.linodeobjects.com",         // Loki: bare host
		region:         "us-ord-1",                           // OBJ cluster id
		harborBucket:   "platform-harbor-registry-primary",   // registry bucket
		harborEndpoint: "https://us-ord-1.linodeobjects.com", // Harbor: full URL
	} {
		if got != want {
			t.Errorf("objectStoreWiring = %q, want %q", got, want)
		}
	}
	// Empty OBJ cluster → all blank (setStr then no-ops, leaving the placeholder).
	c, _, _, _, _, _, _ := objectStoreWiring("dev", "")
	if c != "" {
		t.Errorf("empty objCluster should yield blank wiring, got %q", c)
	}
}

func TestValuesIdentity_RepoURLDefaultsToInstanceRepo(t *testing.T) {
	lz := &LandingZone{}
	lz.Spec.Instance.Repo = "acme/platform-instance"
	lz.Spec.Environments = map[string]Environment{"e2e": func() Environment {
		var e Environment
		e.Cluster.Bootstrap.Name = "platform-e2e"
		e.Cluster.Bootstrap.DomainSuffix = "e2e.internal"
		// aplValues.repoURL deliberately omitted — the common `llz env add` shape.
		return e
	}()}
	id := lz.ValuesIdentity("e2e")
	// Must resolve to the instance repo (the copier tfvars-example default this
	// replaces); an empty RepoURL would leave ${apl_values_repo_url} in the
	// committed values.yaml and hard-fail cluster-bootstrap's secrets-only
	// templatefile() — the release-e2e regression.
	if id.RepoURL != "https://github.com/acme/platform-instance.git" {
		t.Errorf("RepoURL = %q, want the instance-repo default", id.RepoURL)
	}
	// An explicit spec value still wins.
	e := lz.Spec.Environments["e2e"]
	e.Cluster.Bootstrap.AplValues.RepoURL = "https://github.com/acme/values.git"
	lz.Spec.Environments["e2e"] = e
	if id := lz.ValuesIdentity("e2e"); id.RepoURL != "https://github.com/acme/values.git" {
		t.Errorf("explicit RepoURL = %q, want the spec value to win", id.RepoURL)
	}
}

func TestValuesIdentity_DerivedAndDefaults(t *testing.T) {
	lz := &LandingZone{}
	lz.Spec.Environments = map[string]Environment{"primary": func() Environment {
		var e Environment
		e.Cluster.Bootstrap.Name = "platform-primary"
		e.Cluster.Bootstrap.DomainSuffix = "primary.example.com"
		e.Cluster.Bootstrap.AplValues.RepoURL = "https://github.com/acme/platform.git"
		// Username + Revision intentionally omitted → defaults.
		e.Cluster.ObjectStorage.Cluster = "us-ord-1"
		return e
	}()}

	id := lz.ValuesIdentity("primary")
	for got, want := range map[string]string{
		id.ClusterName:      "platform-primary",
		id.LokiBucketChunks: "platform-loki-chunks-primary",
		id.HarborBucket:     "platform-harbor-registry-primary",
		id.LokiS3Endpoint:   "us-ord-1.linodeobjects.com",
		id.HarborS3Endpoint: "https://us-ord-1.linodeobjects.com",
		id.LokiS3Region:     "us-ord-1",
		id.RepoURL:          "https://github.com/acme/platform.git",
		id.RepoUsername:     "x-access-token", // default
		id.RepoBranch:       "main",           // default
	} {
		if got != want {
			t.Errorf("ValuesIdentity field = %q, want %q", got, want)
		}
	}
}

func TestRenderValues_ObjectStoreAndRepo(t *testing.T) {
	const base = `apps:
  loki:
    enabled: true
    _rawValues:
      loki:
        storage:
          bucketNames:
            chunks: ${loki_bucket_chunks}
            ruler:  ${loki_bucket_ruler}
            admin:  ${loki_bucket_admin}
          s3:
            endpoint: ${loki_s3_endpoint}
            region:   ${loki_s3_region}
  harbor:
    enabled: true
    _rawValues:
      persistence:
        imageChartStorage:
          s3:
            bucket:         ${harbor_bucket}
            region:         ${harbor_s3_region}
            regionendpoint: ${harbor_s3_endpoint}
otomi:
  git:
    repoUrl:  ${apl_values_repo_url}
    username: ${apl_values_repo_username}
    branch:   ${apl_values_repo_ref}
    password: ${apl_values_repo_password}
`
	id := ValuesIdentity{
		LokiBucketChunks: "platform-loki-chunks-primary",
		LokiBucketRuler:  "platform-loki-ruler-primary",
		LokiBucketAdmin:  "platform-loki-admin-primary",
		LokiS3Endpoint:   "us-ord-1.linodeobjects.com",
		LokiS3Region:     "us-ord-1",
		HarborBucket:     "platform-harbor-registry-primary",
		HarborS3Endpoint: "https://us-ord-1.linodeobjects.com",
		HarborS3Region:   "us-ord-1",
		RepoURL:          "https://github.com/acme/platform.git",
		RepoUsername:     "x-access-token",
		RepoBranch:       "main",
	}
	out, err := RenderValues([]byte(base), nil, id)
	if err != nil {
		t.Fatalf("RenderValues: %v", err)
	}
	s := string(out)
	for _, w := range []string{
		"chunks: platform-loki-chunks-primary",
		"ruler: platform-loki-ruler-primary",
		"admin: platform-loki-admin-primary",
		"endpoint: us-ord-1.linodeobjects.com",
		"bucket: platform-harbor-registry-primary",
		"regionendpoint: https://us-ord-1.linodeobjects.com",
		"repoUrl: https://github.com/acme/platform.git",
		"username: x-access-token",
		"branch: main",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("object-store/repo not rendered: missing %q:\n%s", w, s)
		}
	}
	// The genuine secret placeholder is left for Terraform's templatefile().
	if !strings.Contains(s, "${apl_values_repo_password}") {
		t.Errorf("secret placeholder must be preserved for templatefile():\n%s", s)
	}
	// No derivable placeholder should survive.
	for _, ph := range []string{"${loki_bucket_chunks}", "${harbor_bucket}", "${apl_values_repo_url}", "${apl_values_repo_ref}"} {
		if strings.Contains(s, ph) {
			t.Errorf("derivable placeholder %q should be resolved, still present:\n%s", ph, s)
		}
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

// TestRenderValuesAlerting proves spec.alerting rewrites the base alerts:
// block (receivers list + slack channels) and that an unset spec keeps the
// base's safe default (receivers: [none]).
func TestRenderValuesAlerting(t *testing.T) {
	base := `apps:
  alertmanager: { enabled: true }
alerts:
  receivers: ["none"]
  slack:
    channel: mon-apl
    channelCrit: mon-apl-crit
`
	out, err := RenderValues([]byte(base), nil, ValuesIdentity{
		AlertReceivers:        []string{"slack"},
		AlertSlackChannel:     "plat-alerts",
		AlertSlackChannelCrit: "plat-alerts-crit",
	})
	if err != nil {
		t.Fatalf("RenderValues: %v", err)
	}
	s := string(out)
	for _, w := range []string{"- slack", "channel: plat-alerts", "channelCrit: plat-alerts-crit"} {
		if !strings.Contains(s, w) {
			t.Errorf("alerting not rendered: missing %q:\n%s", w, s)
		}
	}
	if strings.Contains(s, "none") {
		t.Errorf("receivers must be replaced, none still present:\n%s", s)
	}

	// Unset spec → the base default survives untouched.
	out, err = RenderValues([]byte(base), nil, ValuesIdentity{})
	if err != nil {
		t.Fatalf("RenderValues (defaults): %v", err)
	}
	s = string(out)
	if !strings.Contains(s, "none") {
		t.Errorf("unset spec must keep receivers [none]:\n%s", s)
	}
	if !strings.Contains(s, "channel: mon-apl") {
		t.Errorf("unset spec must keep the base slack channel:\n%s", s)
	}
}

// TestValidateAlerting pins the spec surface: slack|none only, no mixing
// none with a real channel, and channel overrides require the slack receiver.
func TestValidateAlerting(t *testing.T) {
	cases := []struct {
		name    string
		a       Alerting
		wantErr string // "" = valid
	}{
		{"unset is valid", Alerting{}, ""},
		{"none is valid", Alerting{Receivers: []string{"none"}}, ""},
		{"slack with channels is valid", Alerting{
			Receivers: []string{"slack"},
			Slack:     AlertingSlack{Channel: "a", ChannelCrit: "b"},
		}, ""},
		{"msteams rejected", Alerting{Receivers: []string{"msteams"}}, "not supported"},
		{"none plus slack rejected", Alerting{Receivers: []string{"none", "slack"}}, "cannot be combined"},
		{"channels without slack receiver rejected", Alerting{
			Slack: AlertingSlack{Channel: "a"},
		}, "does not include slack"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateAlerting(tc.a)
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want valid, got %v", errs)
				}
				return
			}
			found := false
			for _, e := range errs {
				if strings.Contains(e.Error(), tc.wantErr) {
					found = true
				}
			}
			if !found {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, errs)
			}
		})
	}
}
