package clusterspec

import (
	"strings"
	"testing"
)

// validSpec is a minimal but complete LandingZone used as the base for the
// table tests; helpers tweak one thing at a time.
const validSpec = `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata:
  name: my-instance
spec:
  instance:
    upstreamOrg: akamai-consulting
    repo: my-org/my-instance
    forge: github
    templateVersion: v0.4.0
  environments:
    primary:
      cluster:
        clusterLabel: platform-support
        region: us-ord
        k8sVersion: v1.33.6+lke7
        tags: [platform, observability]
        nodePool: { type: g8-dedicated-8-4, count: 5 }
        controlPlane: { highAvailability: true, auditLogsEnabled: true }
        ha: { role: standalone }
        bootstrap:
          name: platform-primary
          domainSuffix: primary.example.com
        objectStorage: { cluster: us-ord-1 }
`

func mustDecode(t *testing.T, y string) *LandingZone {
	t.Helper()
	lz, err := Decode([]byte(y))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return lz
}

func TestDecodeAndValidate_OK(t *testing.T) {
	lz := mustDecode(t, validSpec)
	if errs := lz.Validate(); len(errs) != 0 {
		t.Fatalf("expected valid spec, got errors: %v", errs)
	}
}

func TestDefaults_RecipesAndDomain(t *testing.T) {
	// A spec env with no recipes and no domainSuffix should get the full
	// default recipe set (dns disabled) and the <env>.internal domain.
	const y = `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
  environments:
    lab:
      cluster:
        clusterLabel: lab
        region: us-sea
        k8sVersion: v1.33.6+lke7
        nodePool: { type: g8-dedicated-8-4, count: 3 }
        bootstrap: { name: platform-lab }
        objectStorage: { cluster: us-sea-1 }
`
	lz := mustDecode(t, y)
	env := lz.Spec.Environments["lab"]
	if got := env.Cluster.Bootstrap.DomainSuffix; got != "lab.internal" {
		t.Errorf("domainSuffix default = %q, want lab.internal", got)
	}
	if !env.Recipes["openbao"].Enabled {
		t.Error("openbao recipe should default enabled")
	}
	if env.Recipes["dns"].Enabled {
		t.Error("dns recipe should default disabled")
	}
	if errs := lz.Validate(); len(errs) != 0 {
		t.Fatalf("defaulted spec should validate, got: %v", errs)
	}
}

func TestDefaults_PartialRecipesPreserveExplicitFalse(t *testing.T) {
	const y = validSpec
	lz := mustDecode(t, y+`      recipes:
        harbor: { enabled: false }
`)
	env := lz.Spec.Environments["primary"]
	if env.Recipes["harbor"].Enabled {
		t.Error("explicit harbor:false must be preserved by Defaults")
	}
	if !env.Recipes["observability"].Enabled {
		t.Error("unmentioned recipe should default enabled")
	}
}

func TestValidate_Errors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  string // appended under the primary cluster/recipes
		wantSub string
	}{
		{"unknown recipe", "      recipes:\n        bogus: { enabled: true }\n", "unknown recipe"},
		{"mandatory disabled", "      recipes:\n        argocd: { enabled: false }\n", "mandatory"},
		{"openbao missing dep", "      recipes:\n        externalSecrets: { enabled: false }\n", "requires recipe \"externalSecrets\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lz := mustDecode(t, validSpec+tc.mutate)
			errs := lz.Validate()
			if !containsSub(errs, tc.wantSub) {
				t.Fatalf("want an error containing %q; got %v", tc.wantSub, errs)
			}
		})
	}
}

func TestValidate_HAInconsistent(t *testing.T) {
	const y = `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
  environments:
    a:
      cluster:
        clusterLabel: a
        region: us-ord
        k8sVersion: v1
        nodePool: { type: t, count: 3 }
        ha: { role: active }
        bootstrap: { name: platform-a }
        objectStorage: { cluster: us-ord-1 }
`
	lz := mustDecode(t, y)
	if !containsSub(lz.Validate(), "requires") {
		t.Fatalf("active role without group should error; got %v", lz.Validate())
	}
}

func TestValidate_BadForgeAndAPIVersion(t *testing.T) {
	y := strings.Replace(validSpec, "forge: github", "forge: bitbucket", 1)
	y = strings.Replace(y, APIVersion, "v0", 1)
	lz := mustDecode(t, y)
	errs := lz.Validate()
	if !containsSub(errs, "forge") || !containsSub(errs, "apiVersion") {
		t.Fatalf("want forge + apiVersion errors; got %v", errs)
	}
}

func TestDecode_RejectsUnknownField(t *testing.T) {
	if _, err := Decode([]byte(validSpec + "  bogusTopLevel: true\n")); err == nil {
		t.Fatal("UnmarshalStrict should reject an unknown field")
	}
}

func TestClusterTFVars_Golden(t *testing.T) {
	lz := mustDecode(t, validSpec)
	got := assignMap(ClusterTFVars(lz.Spec.Environments["primary"].Cluster))
	want := map[string]string{
		"cluster_label":                    `"platform-support"`,
		"region":                           `"us-ord"`,
		"k8s_version":                      `"v1.33.6+lke7"`,
		"tags":                             `["platform", "observability"]`,
		"node_type":                        `"g8-dedicated-8-4"`,
		"node_count":                       `5`,
		"control_plane_high_availability":  `true`,
		"control_plane_audit_logs_enabled": `true`,
		"ha_role":                          `"standalone"`,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("cluster tfvars %s = %q, want %q", k, got[k], v)
		}
	}
	// Omitted optionals must NOT be emitted (left to the example default).
	for _, k := range []string{"autoscaler_enabled", "github_runner_ipv4_cidrs", "ha_group", "promotion_rank"} {
		if _, ok := got[k]; ok {
			t.Errorf("omitted field %s should not be assigned, got %q", k, got[k])
		}
	}
}

func TestBootstrapAndObjTFVars_InjectEnvName(t *testing.T) {
	lz := mustDecode(t, validSpec)
	c := lz.Spec.Environments["primary"].Cluster
	b := assignMap(BootstrapTFVars("primary", c))
	if b["deployment"] != `"primary"` || b["apl_values_env"] != `"primary"` {
		t.Errorf("deployment/apl_values_env must be the env name; got %v", b)
	}
	if b["cluster_name"] != `"platform-primary"` || b["cluster_domain"] != `"primary.example.com"` {
		t.Errorf("bootstrap name/domain mismatch; got %v", b)
	}
	o := assignMap(ObjectStorageTFVars("primary", c))
	if o["region_suffix"] != `"primary"` || o["obj_cluster"] != `"us-ord-1"` {
		t.Errorf("object-storage mapping mismatch; got %v", o)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func assignMap(as []Assign) map[string]string {
	m := make(map[string]string, len(as))
	for _, a := range as {
		m[a.Key] = a.Val
	}
	return m
}

func containsSub(errs []error, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), sub) {
			return true
		}
	}
	return false
}
