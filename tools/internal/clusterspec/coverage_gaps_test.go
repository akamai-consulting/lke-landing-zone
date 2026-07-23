package clusterspec

// Coverage for the pure spec accessors / renderers / mapping helpers that were
// previously exercised only indirectly (or not at all), plus the validator error
// branches. All deterministic — no filesystem or network.

import (
	"strings"
	"testing"
)

func TestComponentBackends(t *testing.T) {
	cases := []struct {
		name string
		c    Component
		want []string
	}{
		{"apl-core only", Component{AplCoreApps: []string{"prometheus"}}, []string{"apl-core"}},
		{"llz-argo via manifest", Component{ManifestResources: []string{"x.yaml"}}, []string{"llz-argo"}},
		{"llz-argo via argo apps", Component{ArgoApps: []string{"a.yaml"}}, []string{"llz-argo"}},
		{"llz-argo via patch", Component{Patches: []Patch{{Path: "p.yaml"}}}, []string{"llz-argo"}},
		{"both backends", Component{AplCoreApps: []string{"loki"}, ArgoApps: []string{"a.yaml"}}, []string{"apl-core", "llz-argo"}},
		{"marker only", Component{Name: "marker"}, nil},
	}
	for _, tc := range cases {
		got := tc.c.Backends()
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: Backends() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestComponentKnobs(t *testing.T) {
	if got := ComponentKnobs("observability"); strings.Join(got, ",") != "retention,storage,replicas" {
		t.Errorf("observability knobs = %v", got)
	}
	if got := ComponentKnobs("argocd"); got != nil {
		t.Errorf("argocd should have no knobs, got %v", got)
	}
}

func TestKnobList(t *testing.T) {
	if got := knobList("observability"); got != "retention, storage, replicas" {
		t.Errorf("knobList(observability) = %q", got)
	}
	if got := knobList("argocd"); got != "(none)" {
		t.Errorf("knobList(argocd) = %q, want (none)", got)
	}
}

func TestHclBool(t *testing.T) {
	if hclBool(true) != "true" || hclBool(false) != "false" {
		t.Error("hclBool wrong")
	}
}

func TestPlatformHasExternal(t *testing.T) {
	tru, fls := true, false
	// ExternalDNS defaults true; ExternalIDP defaults false.
	if !(Platform{}).HasExternalDNS() {
		t.Error("HasExternalDNS default should be true")
	}
	if (Platform{}).HasExternalIDP() {
		t.Error("HasExternalIDP default should be false")
	}
	if (Platform{ExternalDNS: &fls}).HasExternalDNS() {
		t.Error("explicit ExternalDNS=false should be false")
	}
	if !(Platform{ExternalIDP: &tru}).HasExternalIDP() {
		t.Error("explicit ExternalIDP=true should be true")
	}
}

func TestLandingZoneEnv(t *testing.T) {
	lz := mustDecode(t, validSpec)
	if _, ok := lz.Env("primary"); !ok {
		t.Error("primary env should exist")
	}
	if _, ok := lz.Env("ghost"); ok {
		t.Error("ghost env should not exist")
	}
}

func TestValuesIdentity(t *testing.T) {
	lz := mustDecode(t, validSpec)
	id := lz.ValuesIdentity("primary")
	if id.ClusterName != "platform-primary" {
		t.Errorf("ClusterName = %q, want platform-primary", id.ClusterName)
	}
	if id.DomainSuffix != "primary.example.com" {
		t.Errorf("DomainSuffix = %q, want primary.example.com", id.DomainSuffix)
	}
	if !id.ExternalDNS || id.ExternalIDP { // spec defaults: DNS on, IDP off
		t.Errorf("platform flags = (dns:%v idp:%v), want (true,false)", id.ExternalDNS, id.ExternalIDP)
	}
}

func TestNetworkTFVars(t *testing.T) {
	got := NetworkTFVars("shared-ord", VPC{Region: "us-ord"})
	want := []Assign{{Key: "vpc_label", Val: `"shared-ord"`}, {Key: "region", Val: `"us-ord"`}}
	if len(got) != len(want) {
		t.Fatalf("NetworkTFVars len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("assign[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRenderEnvRevision(t *testing.T) {
	out := RenderEnvRevision("abc123")
	for _, want := range []string{"name: env-revision", "revision: abc123", "local-config"} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderEnvRevision missing %q:\n%s", want, out)
		}
	}
}

func TestRenderOtelSANPatch(t *testing.T) {
	out := RenderOtelSANPatch("primary")
	for _, want := range []string{
		"kind: Certificate",
		"name: platform-otel-collector-tls",
		"namespace: llz-observability",
		"- otel.primary.internal",
		// The patch replaces spec.dnsNames wholesale (CR lists are atomic under
		// kustomize), so the static Service SANs must ride along with the env one.
		"- platform-otel-collector.llz-observability.svc\n",
		"- platform-otel-collector.llz-observability.svc.cluster.local",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderOtelSANPatch missing %q:\n%s", want, out)
		}
	}
}

func TestCidrsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"10.0.0.0/13", "10.4.0.0/14", true},  // nested
		{"10.0.0.0/14", "10.8.0.0/14", false}, // disjoint
		{"10.0.0.0/13", "10.0.0.0/13", true},  // identical
		{"not-a-cidr", "10.0.0.0/14", false},  // unparseable → non-overlapping
	}
	for _, c := range cases {
		if got := cidrsOverlap(c.a, c.b); got != c.want {
			t.Errorf("cidrsOverlap(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestValidateInstance_Errors covers the required-field error branches.
func TestValidateInstance_Errors(t *testing.T) {
	errs := validateInstance(Instance{}) // everything empty + invalid forge
	joined := errsString(errs)
	for _, want := range []string{"upstreamOrg is required", "repo is required", "templateVersion is required"} {
		if !strings.Contains(joined, want) {
			t.Errorf("validateInstance missing %q in: %s", want, joined)
		}
	}
}

// TestValidateEnv_Errors covers the required-field branches of validateEnv via an
// empty Environment.
func TestValidateEnv_Errors(t *testing.T) {
	errs := validateEnv("bad", Environment{})
	joined := errsString(errs)
	for _, want := range []string{
		"cluster.clusterLabel is required",
		"cluster.region is required",
		"cluster.k8sVersion is required",
		"cluster.nodePool.type is required",
		"cluster.nodePool.count must be > 0",
		"cluster.bootstrap.name is required",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("validateEnv missing %q in: %s", want, joined)
		}
	}
}

// wedgeEnv is a minimal-but-valid Environment for exercising the branch-collision
// guard in isolation — every OTHER required field is filled so the only finding under
// test is the branch one.
func wedgeEnv(appsRev, aplRev string) Environment {
	return Environment{Cluster: Cluster{
		ClusterLabel: "x", Region: "us-ord", K8sVersion: "1.31",
		NodePool:  NodePool{Type: "g6-standard-4", Count: 3},
		Bootstrap: Bootstrap{Name: "c", AppsRepoRevision: appsRev, AplValues: AplValues{Revision: aplRev}},
	}}
}

func hasBranchWedge(errs []error) bool {
	return strings.Contains(errsString(errs), "reproduces the pre-ADR converge wedge")
}

// The apl-owned branch (apl-<env> by default) and the LLZ apps branch (main by
// default) must resolve to different branches, or apl-operator's env/ commits land on
// the branch platform-bootstrap reads — the pre-ADR wedge.
func TestValidateEnv_BranchWedge(t *testing.T) {
	// Defaults never collide: apl-lab vs main.
	if hasBranchWedge(validateEnv("lab", wedgeEnv("", ""))) {
		t.Error("defaults (apl-lab vs main) must not trip the wedge guard")
	}
	// The classic reintroduction: point the apl branch at main.
	if !hasBranchWedge(validateEnv("lab", wedgeEnv("", "main"))) {
		t.Error("aplValues.revision=main (== default appsRepoRevision) must be flagged")
	}
	// The mirror: point the apps branch at the apl-owned branch.
	if !hasBranchWedge(validateEnv("lab", wedgeEnv("apl-lab", ""))) {
		t.Error("appsRepoRevision=apl-lab (== default aplValues branch) must be flagged")
	}
	// Both set to the same custom branch — neither is a default, still a collision.
	if !hasBranchWedge(validateEnv("lab", wedgeEnv("shared", "shared"))) {
		t.Error("two explicit values resolving equal must be flagged")
	}
	// Two distinct explicit branches are fine.
	if hasBranchWedge(validateEnv("lab", wedgeEnv("release-1", "apl-lab"))) {
		t.Error("distinct explicit branches must not be flagged")
	}
}

func TestBootstrap_BranchDefaults(t *testing.T) {
	var b Bootstrap
	if got := b.AplValuesBranch("prod"); got != "apl-prod" {
		t.Errorf("AplValuesBranch default = %q, want apl-prod", got)
	}
	if got := b.AppsRevision(); got != "main" {
		t.Errorf("AppsRevision default = %q, want main", got)
	}
	b = Bootstrap{AppsRepoRevision: "v1", AplValues: AplValues{Revision: "b2"}}
	if b.AplValuesBranch("prod") != "b2" || b.AppsRevision() != "v1" {
		t.Errorf("explicit values must win: got %q / %q", b.AplValuesBranch("prod"), b.AppsRevision())
	}
}

func errsString(errs []error) string {
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}

func TestRenderReconcilerEnvPatch(t *testing.T) {
	out := RenderReconcilerEnvPatch("exa", "example", "us-ord-1", "acme/instance")
	for _, want := range []string{
		"kind: Deployment", "name: llz-reconciler", "name: reconcile",
		"REGION_SHORT", `value: "exa"`, // volume-labels
		"REGION", `value: "example"`, // linode-creds
		"OBJ_CLUSTER", `value: "us-ord-1"`,
		"GH_REPO", `value: "acme/instance"`, // apl-overlay
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderReconcilerEnvPatch missing %q:\n%s", want, out)
		}
	}
}

func TestRenderHarborHostPatch(t *testing.T) {
	out := RenderHarborHostPatch("web.prod.example.com", "acme/inst")
	for _, want := range []string{
		"name: harbor-robot-provisioner",
		"namespace: harbor",
		"name: HARBOR_HOST",
		`value: "harbor.web.prod.example.com"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderHarborHostPatch missing %q:\n%s", want, out)
		}
	}
}
