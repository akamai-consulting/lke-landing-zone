package clusterspec

import (
	"fmt"
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
          managedAppPlatform: true
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

func TestDefaults_ComponentsAndDomain(t *testing.T) {
	// A spec env with no components should get the full default component set. On the
	// managed platform domainSuffix is NEVER defaulted (Linode owns the domain).
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
        bootstrap: { name: platform-lab, managedAppPlatform: true }
        objectStorage: { cluster: us-sea-1 }
`
	lz := mustDecode(t, y)
	env := lz.Spec.Environments["lab"]
	if got := env.Cluster.Bootstrap.DomainSuffix; got != "" {
		t.Errorf("domainSuffix must not be defaulted on managed, got %q", got)
	}
	if !ComponentEnabled(env.Components, "openbao") {
		t.Error("openbao component should default enabled")
	}
	if ComponentEnabled(env.Components, "nonexistent") {
		t.Error("an unknown component should not be enabled")
	}
	if errs := lz.Validate(); len(errs) != 0 {
		t.Fatalf("defaulted spec should validate, got: %v", errs)
	}
}

func TestDefaults_TeamsNotDefaulted(t *testing.T) {
	// New-clusters-only: Defaults() must NOT inject a team, so an existing instance
	// that never declared spec.teams stays team-less (no surprise team provisioned
	// on upgrade — the default is authored at `llz new` scaffold time instead).
	lz := mustDecode(t, validSpec)
	if len(lz.Spec.Teams) != 0 {
		t.Fatalf("Defaults() must not inject teams (new-clusters-only), got %+v", lz.Spec.Teams)
	}
	// A declared team is untouched + validates.
	lz2 := mustDecode(t, validSpec+`  teams:
    - name: gsap
      openbaoSubtree: secret/gsap
`)
	if len(lz2.Spec.Teams) != 1 || lz2.Spec.Teams[0].Name != "gsap" {
		t.Errorf("declared teams should pass through untouched, got %+v", lz2.Spec.Teams)
	}
	if errs := lz2.Validate(); len(errs) != 0 {
		t.Errorf("declared team must validate, got %v", errs)
	}
}

func TestDefaults_PartialComponentsPreserveExplicitFalse(t *testing.T) {
	const y = validSpec
	lz := mustDecode(t, y+`      components:
        harbor: { enabled: false }
`)
	env := lz.Spec.Environments["primary"]
	if ComponentEnabled(env.Components, "harbor") {
		t.Error("explicit harbor:false must be preserved by Defaults")
	}
	if !ComponentEnabled(env.Components, "observability") {
		t.Error("unmentioned component should default enabled")
	}
}

// A tune-only toggle (sizing set, enabled omitted) must NOT read as a disable.
func TestComponents_TuneOnlyDoesNotDisable(t *testing.T) {
	lz := mustDecode(t, validSpec+`      components:
        observability: { retention: 30d }
`)
	env := lz.Spec.Environments["primary"]
	if !ComponentEnabled(env.Components, "observability") {
		t.Error("tune-only toggle (retention set, enabled omitted) must keep the component enabled")
	}
	if got := env.Components["observability"].Retention; got != "30d" {
		t.Errorf("retention = %q, want 30d", got)
	}
	if errs := lz.Validate(); len(errs) != 0 {
		t.Fatalf("tune-only spec should validate: %v", errs)
	}
}

// Field-level merge: a default's enabled state survives an env-level tune-only
// override (env sets only sizing; the default's enabled:false still wins).
func TestComponents_MergeTriState(t *testing.T) {
	lz := mustDecode(t, `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
  defaults:
    components:
      observability: { enabled: false }
  environments:
    primary:
      cluster:
        clusterLabel: c
        region: us-ord
        k8sVersion: v1.33.6+lke7
        nodePool: { type: t, count: 3 }
        bootstrap: { name: c, managedAppPlatform: true }
        objectStorage: { cluster: us-ord-1 }
      components:
        observability: { retention: 30d }
`)
	env := lz.Spec.Environments["primary"]
	if ComponentEnabled(env.Components, "observability") {
		t.Error("default enabled:false must survive an env tune-only override")
	}
	if got := env.Components["observability"].Retention; got != "30d" {
		t.Errorf("retention = %q, want 30d (env override)", got)
	}
}

func TestValidate_Errors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  string // appended under the primary cluster/components
		wantSub string
	}{
		{"unknown component", "      components:\n        bogus: { enabled: true }\n", "unknown component"},
		{"mandatory disabled", "      components:\n        argocd: { enabled: false }\n", "mandatory"},
		{"openbao missing dep", "      components:\n        externalSecrets: { enabled: false }\n", "requires component \"externalSecrets\""},
		{"vpc cidr bad prefix", "        network: { subnetCIDR: 10.0.0.0/24 }\n", "/13 or /14"},
		{"vpc cidr not a cidr", "        network: { subnetCIDR: nope }\n", "not a valid CIDR"},
		{"sizing knob wrong component", "      components:\n        harbor: { retention: 30d }\n", "retention is not a valid setting"},
		{"broadPAT knob wrong component", "      components:\n        harbor: { broadPATLabel: x }\n", "broadPATLabel is not a valid setting"},
		{"broadPatRotator enabled without config", "      components:\n        broadPatRotator: { enabled: true }\n", "requires broadPATLabel and broadPATDeployments"},
		{"bad retention format", "      components:\n        observability: { retention: 30days }\n", "is not a duration"},
		{"bad storage quantity", "      components:\n        observability: { storage: 50GB }\n", "is not a storage quantity"},
		{"replicas below one", "      components:\n        observability: { replicas: 0 }\n", "replicas must be >= 1"},
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

// A recognized-but-not-yet-wired forge must be rejected loudly (the Phase 0
// honesty gate), not silently accepted the way spec.instance.forge used to be.
func TestValidate_RecognizedForgeNotYetSupported(t *testing.T) {
	for _, fl := range []string{"github-enterprise", "github-enterprise-server", "gitlab"} {
		y := strings.Replace(validSpec, "forge: github", "forge: "+fl, 1)
		errs := mustDecode(t, y).Validate()
		if !containsSub(errs, "not yet supported") {
			t.Errorf("forge %q: want a not-yet-supported error; got %v", fl, errs)
		}
	}
	// github stays clean.
	if errs := mustDecode(t, validSpec).Validate(); len(errs) != 0 {
		t.Errorf("forge github should validate clean; got %v", errs)
	}
}

// haPairSpec builds an active/standby HA pair in two regions; an empty cidr omits
// the env's network block (so it falls back to the default).
func haPairSpec(cidrA, cidrB string) string {
	net := func(c string) string {
		if c == "" {
			return ""
		}
		return "\n        network: { subnetCIDR: " + c + " }"
	}
	return `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
  environments:
    east:
      cluster:
        clusterLabel: east
        region: us-ord
        k8sVersion: v1.33.6+lke7
        nodePool: { type: t, count: 3 }
        ha: { role: active, group: prod }` + net(cidrA) + `
        bootstrap: { name: platform-east, managedAppPlatform: true }
        objectStorage: { cluster: us-ord-1 }
    west:
      cluster:
        clusterLabel: west
        region: us-sea
        k8sVersion: v1.33.6+lke7
        nodePool: { type: t, count: 3 }
        ha: { role: standby, group: prod }` + net(cidrB) + `
        bootstrap: { name: platform-west, managedAppPlatform: true }
        objectStorage: { cluster: us-sea-1 }
`
}

func TestValidate_HAVPCOverlap(t *testing.T) {
	overlapping := map[string][2]string{
		"both default (silent collision)": {"", ""},
		"explicit identical":              {"10.0.0.0/13", "10.0.0.0/13"},
		"nested":                          {"10.0.0.0/13", "10.0.0.0/14"},
	}
	for name, c := range overlapping {
		t.Run(name, func(t *testing.T) {
			if !containsSub(mustDecode(t, haPairSpec(c[0], c[1])).Validate(), "overlapping VPC subnet") {
				t.Errorf("HA peers (%q,%q) should overlap-error", c[0], c[1])
			}
		})
	}
	// Distinct, non-overlapping /13s validate clean.
	if errs := mustDecode(t, haPairSpec("10.0.0.0/13", "10.8.0.0/13")).Validate(); len(errs) != 0 {
		t.Errorf("distinct HA CIDRs should validate clean; got %v", errs)
	}
}

func TestValidate_Networks(t *testing.T) {
	// base has a shared VPC "ord" (us-ord) and two envs; the verb args fill web's
	// subnet, then api's region / vpc-ref / subnet.
	base := `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
  networks:
    ord: { region: us-ord }
  environments:
    web:
      cluster:
        clusterLabel: web
        region: us-ord
        k8sVersion: v1.33.6+lke7
        nodePool: { type: t, count: 3 }
        network: { vpc: ord, subnetCIDR: %s }
        bootstrap: { name: platform-web, managedAppPlatform: true }
        objectStorage: { cluster: us-ord-1 }
    api:
      cluster:
        clusterLabel: api
        region: %s
        k8sVersion: v1.33.6+lke7
        nodePool: { type: t, count: 3 }
        network: { vpc: %s, subnetCIDR: %s }
        bootstrap: { name: platform-api, managedAppPlatform: true }
        objectStorage: { cluster: us-ord-1 }
`
	// distinct subnets sharing one VPC → clean.
	if errs := mustDecode(t, fmt.Sprintf(base, "10.0.0.0/14", "us-ord", "ord", "10.4.0.0/14")).Validate(); len(errs) != 0 {
		t.Errorf("distinct subnets in one VPC should validate clean; got %v", errs)
	}
	// overlapping subnets sharing one VPC → error.
	if !containsSub(mustDecode(t, fmt.Sprintf(base, "10.0.0.0/13", "us-ord", "ord", "10.0.0.0/13")).Validate(), "overlapping subnet") {
		t.Error("overlapping subnets in one VPC should error")
	}
	// reference to an undeclared network → error.
	if !containsSub(mustDecode(t, fmt.Sprintf(base, "10.0.0.0/14", "us-ord", "nope", "10.4.0.0/14")).Validate(), "not declared in spec.networks") {
		t.Error("unknown vpc ref should error")
	}
	// attaching to a VPC in another region → error.
	if !containsSub(mustDecode(t, fmt.Sprintf(base, "10.0.0.0/14", "us-sea", "ord", "10.4.0.0/14")).Validate(), "cannot span regions") {
		t.Error("region mismatch should error")
	}
}

func TestClusterTFVars_SubnetCIDR(t *testing.T) {
	got := assignMap(ClusterTFVars(Cluster{Network: ClusterNetwork{SubnetCIDR: "10.8.0.0/14"}}))
	if got["vpc_subnet_cidr"] != `"10.8.0.0/14"` {
		t.Errorf("vpc_subnet_cidr = %q, want \"10.8.0.0/14\"", got["vpc_subnet_cidr"])
	}
	// Unset → not emitted, so the tfvars-example default stands.
	if _, ok := assignMap(ClusterTFVars(Cluster{}))["vpc_subnet_cidr"]; ok {
		t.Error("unset subnetCIDR should not emit vpc_subnet_cidr")
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
	for _, k := range []string{"autoscaler_enabled", "github_runner_ipv4_cidrs", "ha_group"} {
		if _, ok := got[k]; ok {
			t.Errorf("omitted field %s should not be assigned, got %q", k, got[k])
		}
	}
}

func TestObjTFVars_InjectEnvName(t *testing.T) {
	lz := mustDecode(t, validSpec)
	c := lz.Spec.Environments["primary"].Cluster
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
