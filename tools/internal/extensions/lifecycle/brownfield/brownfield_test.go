package brownfield

import (
	"reflect"
	"testing"
)

func TestParseServerVersion(t *testing.T) {
	if got := parseServerVersion(`{"serverVersion":{"gitVersion":"v1.28.7"}}`); got != "v1.28.7" {
		t.Errorf("got %q, want v1.28.7", got)
	}
	if got := parseServerVersion(`not json`); got != "" {
		t.Errorf("garbage should yield empty, got %q", got)
	}
}

func TestParseNodes(t *testing.T) {
	js := `{"items":[
		{"metadata":{"labels":{"node.kubernetes.io/instance-type":"g6-standard-4","topology.kubernetes.io/region":"us-ord"}}},
		{"metadata":{"labels":{"node.kubernetes.io/instance-type":"g6-standard-4","topology.kubernetes.io/region":"us-ord"}}},
		{"metadata":{"labels":{"beta.kubernetes.io/instance-type":"g6-standard-8","failure-domain.beta.kubernetes.io/region":"us-ord"}}}
	]}`
	count, nodeType, region := parseNodes(js)
	if count != 3 {
		t.Errorf("count=%d, want 3", count)
	}
	if nodeType != "g6-standard-4" { // majority of 3
		t.Errorf("nodeType=%q, want g6-standard-4", nodeType)
	}
	if region != "us-ord" {
		t.Errorf("region=%q, want us-ord", region)
	}
	if c, nt, r := parseNodes(`bad`); c != 0 || nt != "" || r != "" {
		t.Errorf("garbage should yield zero values, got %d %q %q", c, nt, r)
	}
}

func TestMostCommonTieBreak(t *testing.T) {
	// equal counts → lexicographically smallest wins, deterministically.
	got := mostCommon(map[string]int{"b": 2, "a": 2, "c": 1})
	if got != "a" {
		t.Errorf("got %q, want a (lexicographic tie-break)", got)
	}
	if mostCommon(map[string]int{}) != "" {
		t.Error("empty map should yield empty string")
	}
}

func TestParseWorkloads(t *testing.T) {
	js := `{"items":[
		{"kind":"Deployment","metadata":{"name":"web","namespace":"team-demo"},
		 "spec":{"template":{"spec":{"containers":[{"image":"nginx:1.25"},{"image":"sidecar:1"}]}}}},
		{"kind":"StatefulSet","metadata":{"name":"db","namespace":"team-demo"},
		 "spec":{"template":{"spec":{"containers":[{"image":"postgres:16"}]}}}}
	]}`
	got := parseWorkloads(js)
	if len(got) != 2 {
		t.Fatalf("got %d workloads, want 2", len(got))
	}
	if got[0].Kind != "Deployment" || got[0].Name != "web" || got[0].Namespace != "team-demo" {
		t.Errorf("unexpected first workload: %+v", got[0])
	}
	if !reflect.DeepEqual(got[0].Images, []string{"nginx:1.25", "sidecar:1"}) {
		t.Errorf("images=%v", got[0].Images)
	}
	if parseWorkloads(`bad`) != nil {
		t.Error("garbage should yield nil")
	}
}

func TestParseIngressHosts(t *testing.T) {
	js := `{"items":[
		{"metadata":{"namespace":"team-demo"},"spec":{"rules":[{"host":"app.demo.example.com"},{"host":""}]}},
		{"metadata":{"namespace":"team-demo"},"spec":{"rules":[{"host":"api.demo.example.com"}]}}
	]}`
	got := parseIngressHosts(js)
	if !reflect.DeepEqual(got["team-demo"], []string{"app.demo.example.com", "api.demo.example.com"}) {
		t.Errorf("hosts=%v", got["team-demo"])
	}
}

func TestParsePVCs(t *testing.T) {
	js := `{"items":[
		{"metadata":{"namespace":"team-demo"},"spec":{"storageClassName":"linode-block-storage","resources":{"requests":{"storage":"8Gi"}}}}
	]}`
	got := parsePVCs(js)
	if len(got) != 1 || got[0].Size != "8Gi" || got[0].StorageClass != "linode-block-storage" {
		t.Errorf("pvc=%+v", got)
	}
}

func TestParseSecretCounts(t *testing.T) {
	js := `{"items":[
		{"metadata":{"namespace":"team-demo"},"type":"Opaque"},
		{"metadata":{"namespace":"team-demo"},"type":"kubernetes.io/tls"},
		{"metadata":{"namespace":"team-demo"},"type":"kubernetes.io/service-account-token"},
		{"metadata":{"namespace":"team-demo"},"type":"helm.sh/release.v1"}
	]}`
	got := parseSecretCounts(js)
	if got["team-demo"] != 2 { // SA token + helm release excluded
		t.Errorf("count=%d, want 2", got["team-demo"])
	}
}

func TestTeamFromNamespace(t *testing.T) {
	cases := []struct {
		ns   string
		name string
		ok   bool
	}{
		{"team-demo", "demo", true},
		{"team-admin", "", false}, // platform team excluded
		{"team-", "", false},
		{"kube-system", "", false},
		{"argocd", "", false},
	}
	for _, c := range cases {
		name, ok := teamFromNamespace(c.ns)
		if name != c.name || ok != c.ok {
			t.Errorf("teamFromNamespace(%q)=(%q,%v), want (%q,%v)", c.ns, name, ok, c.name, c.ok)
		}
	}
}

func TestDetectComponents(t *testing.T) {
	ns := []string{"harbor", "loki", "monitoring", "gitea", "tekton-pipelines", "keycloak", "team-demo", "kube-system"}
	detected, components, warnings := detectComponents(ns, nil)

	wantComponents := map[string]bool{"harbor": true, "observability": true, "gitea": true}
	if !reflect.DeepEqual(components, wantComponents) {
		t.Errorf("components=%v, want %v", components, wantComponents)
	}
	// observability detected once even though loki + monitoring both matched it.
	if !containsStr(detected, "harbor") || !containsStr(detected, "loki") || !containsStr(detected, "monitoring") {
		t.Errorf("detected missing expected apps: %v", detected)
	}
	if len(warnings) != 3 { // gitea + tekton + keycloak
		t.Errorf("warnings=%v, want 3", warnings)
	}
}

// detectComponents always hands back a live map — callers (buildReport) fold the
// CRD-derived components into it. Returning nil here panicked `import scan` on any
// cluster with no recognized platform namespace but a recognized CRD.
func TestDetectComponentsNoneYieldsEmptyNonNilMap(t *testing.T) {
	_, components, warnings := detectComponents([]string{"team-demo", "kube-system"}, nil)
	if components == nil {
		t.Fatal("no platform apps → empty but non-nil component map, got nil")
	}
	if len(components) != 0 {
		t.Errorf("no platform apps → empty component map, got %v", components)
	}
	if len(warnings) != 0 {
		t.Errorf("no warnings expected, got %v", warnings)
	}
}

// A foreign cluster with zero recognized platform namespaces but at least one
// recognized CRD — the exact shape `import scan` exists to survey — used to panic
// with "assignment to entry in nil map" folding the CRD components in.
func TestBuildReportCRDComponentsWithNoPlatformNamespaces(t *testing.T) {
	r := buildReport(reportInputs{
		nsJSON:  `{"items":[{"metadata":{"name":"default"}}]}`,
		crdJSON: `{"items":[{"metadata":{"name":"applications.argoproj.io"},"spec":{"group":"argoproj.io"}}]}`,
	})
	if !r.Platform.Components["argocd"] {
		t.Errorf("CRD-implied component missing: components=%v", r.Platform.Components)
	}
	if len(r.Platform.Detected) != 0 {
		t.Errorf("no platform namespaces → nothing detected, got %v", r.Platform.Detected)
	}
}

// An empty component set still serializes as absent, not as an empty map.
func TestBuildReportEmptyComponentsStayNil(t *testing.T) {
	r := buildReport(reportInputs{nsJSON: `{"items":[{"metadata":{"name":"default"}}]}`})
	if r.Platform.Components != nil {
		t.Errorf("empty component set should be nil, got %v", r.Platform.Components)
	}
}

func TestParseQuantityBytes(t *testing.T) {
	cases := map[string]int64{
		"8Gi":     8 << 30,
		"500Mi":   500 << 20,
		"1Ti":     1 << 40,
		"1000000": 1000000,
		"1.5Gi":   int64(1.5 * float64(int64(1)<<30)),
		"1G":      1e9,
		"":        0,
		"garbage": 0,
	}
	for in, want := range cases {
		if got := parseQuantityBytes(in); got != want {
			t.Errorf("parseQuantityBytes(%q)=%d, want %d", in, got, want)
		}
	}
}

func TestFormatStorage(t *testing.T) {
	cases := map[int64]string{
		0:          "",
		8 << 30:    "8Gi",
		220 << 30:  "220Gi",
		1 << 40:    "1Ti",
		1536 << 20: "1.5Gi",
		512:        "512B",
	}
	for in, want := range cases {
		if got := formatStorage(in); got != want {
			t.Errorf("formatStorage(%d)=%q, want %q", in, got, want)
		}
	}
}

func TestBuildReport(t *testing.T) {
	in := reportInputs{
		context:     "old-apl",
		repos:       []repoInventory{{Role: "apl", Path: "/clones/otomi-values"}},
		versionJSON: `{"serverVersion":{"gitVersion":"v1.28.7"}}`,
		nodesJSON: `{"items":[
			{"metadata":{"labels":{"node.kubernetes.io/instance-type":"g6-standard-4","topology.kubernetes.io/region":"us-ord"}}},
			{"metadata":{"labels":{"node.kubernetes.io/instance-type":"g6-standard-4","topology.kubernetes.io/region":"us-ord"}}}
		]}`,
		nsJSON: `{"items":[
			{"metadata":{"name":"team-demo"}},
			{"metadata":{"name":"team-admin"}},
			{"metadata":{"name":"harbor"}},
			{"metadata":{"name":"loki"}},
			{"metadata":{"name":"kube-system"}}
		]}`,
		workloadJSON: `{"items":[
			{"kind":"Deployment","metadata":{"name":"web","namespace":"team-demo"},"spec":{"template":{"spec":{"containers":[{"image":"nginx:1.25"}]}}}},
			{"kind":"StatefulSet","metadata":{"name":"db","namespace":"team-demo"},"spec":{"template":{"spec":{"containers":[{"image":"postgres:16"}]}}}}
		]}`,
		ingressJSON: `{"items":[
			{"metadata":{"namespace":"team-demo"},"spec":{"rules":[{"host":"app.demo.example.com"}]}}
		]}`,
		pvcJSON: `{"items":[
			{"metadata":{"namespace":"team-demo"},"spec":{"storageClassName":"linode-block-storage","resources":{"requests":{"storage":"8Gi"}}}}
		]}`,
		secretJSON: `{"items":[
			{"metadata":{"namespace":"team-demo"},"type":"Opaque"},
			{"metadata":{"namespace":"team-demo"},"type":"kubernetes.io/service-account-token"}
		]}`,
	}
	r := buildReport(in)

	if r.APIVersion != importReportAPIVersion || r.Kind != importReportKind {
		t.Errorf("bad header: %s/%s", r.APIVersion, r.Kind)
	}
	if r.Source.Context != "old-apl" || len(r.Source.Repos) != 1 {
		t.Errorf("source=%+v", r.Source)
	}
	if r.Cluster.KubernetesVersion != "v1.28.7" || r.Cluster.Region != "us-ord" || r.Cluster.NodeCount != 2 || r.Cluster.NodeType != "g6-standard-4" {
		t.Errorf("cluster=%+v", r.Cluster)
	}
	if !r.Platform.Components["harbor"] || !r.Platform.Components["observability"] {
		t.Errorf("platform components=%v", r.Platform.Components)
	}

	// Exactly one team (team-demo); team-admin and platform namespaces excluded.
	if len(r.Teams) != 1 {
		t.Fatalf("teams=%d, want 1: %+v", len(r.Teams), r.Teams)
	}
	demo := r.Teams[0]
	if demo.Name != "demo" || demo.Namespace != "team-demo" || demo.Workloads != 2 {
		t.Errorf("team=%+v", demo)
	}
	if demo.PVCs != 1 || demo.Storage != "8Gi" || demo.Secrets != 1 {
		t.Errorf("team rollups wrong: %+v", demo)
	}
	if !reflect.DeepEqual(demo.Hosts, []string{"app.demo.example.com"}) {
		t.Errorf("hosts=%v", demo.Hosts)
	}

	if r.Summary.Namespaces != 5 || r.Summary.Workloads != 2 || r.Summary.Hosts != 1 ||
		r.Summary.PVCs != 1 || r.Summary.TotalStorage != "8Gi" || r.Summary.Secrets != 1 {
		t.Errorf("summary=%+v", r.Summary)
	}
}

func TestKubectlCtx(t *testing.T) {
	// The seam is a Deps field now, not a package-level var — so no swap and no
	// cleanup, and two tests could hold different stubs at once.
	var gotArgs []string
	d := Deps{KubectlOut: func(args ...string) (string, error) {
		gotArgs = args
		return "ok", nil
	}}

	// kubeconfig + context are both prepended, in that order.
	if _, err := kubectlCtx(d, "/tmp/kc", "old-apl", "get", "nodes"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotArgs, []string{"--kubeconfig", "/tmp/kc", "--context", "old-apl", "get", "nodes"}) {
		t.Errorf("args=%v, want --kubeconfig /tmp/kc --context old-apl get nodes", gotArgs)
	}

	// Only a kubeconfig, no context.
	if _, err := kubectlCtx(d, "/tmp/kc", "", "get", "pods"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotArgs, []string{"--kubeconfig", "/tmp/kc", "get", "pods"}) {
		t.Errorf("args=%v, want --kubeconfig /tmp/kc get pods", gotArgs)
	}

	// Neither: args untouched (kubectl uses its own defaults).
	if _, err := kubectlCtx(d, "", "", "get", "svc"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotArgs, []string{"get", "svc"}) {
		t.Errorf("args=%v, want get svc", gotArgs)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// Moved with brownfield.go — suggestedInstanceDir reads an importReport, and both
// live here now.

func TestSuggestedInstanceDir(t *testing.T) {
	if got := suggestedInstanceDir(importReport{}); got != "lke-instance" {
		t.Errorf("no Linode section: got %q, want the fallback", got)
	}
	// A present-but-unlabelled cluster must fall back too — the nil check alone
	// is not enough, and an empty label would otherwise become an empty dirname.
	if got := suggestedInstanceDir(importReport{Linode: &importLinode{}}); got != "lke-instance" {
		t.Errorf("empty label: got %q, want the fallback", got)
	}
	if got := suggestedInstanceDir(importReport{Linode: &importLinode{Label: "prod-ord"}}); got != "prod-ord" {
		t.Errorf("labelled cluster: got %q", got)
	}
}

func TestVPCCIDRSummary(t *testing.T) {
	if got := vpcCIDRSummary(nil); got != "" {
		t.Errorf("nil VPC must be empty, got %q", got)
	}
	if got := vpcCIDRSummary(&lkeVPC{}); got != "" {
		t.Errorf("VPC with no subnets must be empty, got %q", got)
	}
	if got := vpcCIDRSummary(&lkeVPC{Subnets: []string{"10.0.0.0/24"}}); got != "10.0.0.0/24" {
		t.Errorf("single subnet: got %q (no trailing separator)", got)
	}
	if got := vpcCIDRSummary(&lkeVPC{Subnets: []string{"10.0.0.0/24", "10.0.1.0/24"}}); got != "10.0.0.0/24,10.0.1.0/24" {
		t.Errorf("multiple subnets: got %q", got)
	}
}

// poolsSuffix annotates a cluster line only when there is more than one pool, so
// the interesting input is exactly one — the boundary.
func TestPoolsSuffix(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{0, ""}, {1, ""}, {2, " (2 pools)"}, {5, " (5 pools)"}} {
		if got := poolsSuffix(make([]nodePool, tc.n)); got != tc.want {
			t.Errorf("poolsSuffix(%d pools) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestFindCluster(t *testing.T) {
	clusters := []map[string]any{
		{"id": float64(101), "label": "a"},
		{"id": float64(202), "label": "b"},
	}
	got, ok := findCluster(clusters, 202)
	if !ok || got["label"] != "b" {
		t.Errorf("findCluster(202) = %v, %v — want the second cluster", got, ok)
	}
	if _, ok := findCluster(clusters, 999); ok {
		t.Error("a missing id must report not-found rather than the zero cluster")
	}
	if _, ok := findCluster(nil, 101); ok {
		t.Error("an empty list must report not-found")
	}
	// Returning the FIRST match matters: ids are unique upstream, but a
	// last-match-wins search would silently pick a different cluster if they
	// ever were not.
	dupes := []map[string]any{{"id": float64(7), "label": "first"}, {"id": float64(7), "label": "second"}}
	if got, _ := findCluster(dupes, 7); got["label"] != "first" {
		t.Errorf("duplicate ids: got %q, want the first match", got["label"])
	}
}

// lkeVPCInfo maps three fields out of an untyped API object. A transposed pair
// is invisible here and surfaces as a VPC attached to the wrong region.
func TestLKEVPCInfo(t *testing.T) {
	got := lkeVPCInfo(map[string]any{"id": float64(42), "label": "vpc-ord", "region": "us-ord"})
	if got.ID != 42 || got.Label != "vpc-ord" || got.Region != "us-ord" {
		t.Errorf("lkeVPCInfo = %+v — fields are transposed or dropped", got)
	}
	// Label and Region are both strings, so a swap type-checks; assert they are
	// not interchangeable.
	if got.Label == got.Region {
		t.Error("label and region resolved to the same value")
	}
	if empty := lkeVPCInfo(map[string]any{}); empty.ID != 0 || empty.Label != "" || empty.Region != "" {
		t.Errorf("missing keys must zero-value, got %+v", empty)
	}
}
