package main

import (
	"reflect"
	"testing"
)

func TestParseNodePools(t *testing.T) {
	js := `{"items":[
		{"metadata":{"labels":{"lke.linode.com/pool-id":"101","node.kubernetes.io/instance-type":"g6-standard-4"}}},
		{"metadata":{"labels":{"lke.linode.com/pool-id":"101","node.kubernetes.io/instance-type":"g6-standard-4"}}},
		{"metadata":{"labels":{"lke.linode.com/pool-id":"202","node.kubernetes.io/instance-type":"g6-dedicated-8"}}}
	]}`
	pools := parseNodePools(js)
	want := []nodePool{
		{PoolID: "101", NodeType: "g6-standard-4", Count: 2},
		{PoolID: "202", NodeType: "g6-dedicated-8", Count: 1},
	}
	if !reflect.DeepEqual(pools, want) {
		t.Errorf("pools=%+v, want %+v", pools, want)
	}
}

func TestParseNodePoolsSinglePoolNil(t *testing.T) {
	// Homogeneous, no pool labels → nil (the majority nodeType already covers it).
	js := `{"items":[
		{"metadata":{"labels":{"node.kubernetes.io/instance-type":"g6-standard-4"}}},
		{"metadata":{"labels":{"node.kubernetes.io/instance-type":"g6-standard-4"}}}
	]}`
	if pools := parseNodePools(js); pools != nil {
		t.Errorf("expected nil for single homogeneous pool, got %+v", pools)
	}
}

func TestParseStorageClasses(t *testing.T) {
	js := `{"items":[
		{"metadata":{"name":"linode-block-storage-retain","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},"provisioner":"linodebs.csi.linode.com"},
		{"metadata":{"name":"linode-block-storage"},"provisioner":"linodebs.csi.linode.com"}
	]}`
	got := parseStorageClasses(js)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[0].Name != "linode-block-storage" || got[0].Default {
		t.Errorf("sorted[0]=%+v", got[0])
	}
	if got[1].Name != "linode-block-storage-retain" || !got[1].Default {
		t.Errorf("sorted[1] should be the default: %+v", got[1])
	}
}

func TestParseIstioHosts(t *testing.T) {
	gw := `{"items":[{"metadata":{"namespace":"istio-system"},"spec":{"servers":[{"hosts":["team-gsap/*.gsap.example.com","*"]}]}}]}`
	vs := `{"items":[{"metadata":{"namespace":"team-gsap"},"spec":{"hosts":["app.gsap.example.com"]}}]}`
	got := parseIstioHosts(gw, vs)
	if !reflect.DeepEqual(got["istio-system"], []string{"gsap.example.com"}) { // ns/ + *. stripped, bare * dropped
		t.Errorf("gateway hosts=%v", got["istio-system"])
	}
	if !reflect.DeepEqual(got["team-gsap"], []string{"app.gsap.example.com"}) {
		t.Errorf("vs hosts=%v", got["team-gsap"])
	}
}

func TestParseCertDNSNames(t *testing.T) {
	js := `{"items":[{"metadata":{"namespace":"team-gsap"},"spec":{"dnsNames":["app.gsap.example.com","*.gsap.example.com"]}}]}`
	got := parseCertDNSNames(js)
	if !reflect.DeepEqual(got["team-gsap"], []string{"app.gsap.example.com", "gsap.example.com"}) {
		t.Errorf("dnsNames=%v", got["team-gsap"])
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"app.example.com":        "app.example.com",
		"*.example.com":          "example.com",
		"ns/host.example.com":    "host.example.com",
		"team-x/*.x.example.com": "x.example.com",
		"*":                      "",
		"*/*":                    "",
		// cluster-internal / non-domain → dropped
		"barman-cloud":                             "",
		"po-operator":                              "",
		"otel-operator-webhook.otel.svc":           "",
		"po-operator.monitoring.svc.cluster.local": "",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestCommonDomainSuffix(t *testing.T) {
	cases := []struct {
		hosts []string
		want  string
	}{
		{[]string{"app.demo.example.com", "api.example.com"}, "example.com"},
		{[]string{"a.example.com", "b.example.com"}, "example.com"},
		{[]string{"a.com", "b.org"}, ""}, // only TLD differs → nothing meaningful
		{[]string{"only.one.host.net"}, "only.one.host.net"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := commonDomainSuffix(c.hosts); got != c.want {
			t.Errorf("commonDomainSuffix(%v)=%q, want %q", c.hosts, got, c.want)
		}
	}
}

func TestParseLoadBalancers(t *testing.T) {
	js := `{"items":[
		{"metadata":{"name":"istio-ingressgateway","namespace":"istio-system"},"spec":{"type":"LoadBalancer"},"status":{"loadBalancer":{"ingress":[{"ip":"203.0.113.10"}]}}},
		{"metadata":{"name":"clusterip-svc","namespace":"x"},"spec":{"type":"ClusterIP"}}
	]}`
	got := parseLoadBalancers(js)
	if len(got) != 1 {
		t.Fatalf("got %d LBs, want 1 (ClusterIP excluded)", len(got))
	}
	if got[0].Name != "istio-ingressgateway" || !reflect.DeepEqual(got[0].Addresses, []string{"203.0.113.10"}) {
		t.Errorf("lb=%+v", got[0])
	}
}

func TestParseCRDOperators(t *testing.T) {
	js := `{"items":[
		{"metadata":{"name":"certificates.cert-manager.io"},"spec":{"group":"cert-manager.io"}},
		{"metadata":{"name":"virtualservices.networking.istio.io"},"spec":{"group":"networking.istio.io"}},
		{"metadata":{"name":"workflows.argoproj.io"},"spec":{"group":"argoproj.io"}},
		{"metadata":{"name":"applications.argoproj.io"},"spec":{"group":"argoproj.io"}},
		{"metadata":{"name":"pipelines.tekton.dev"},"spec":{"group":"tekton.dev"}}
	]}`
	ops, comps := parseCRDOperators(js)
	wantOps := []string{"argo", "cert-manager", "istio", "tekton"}
	if !reflect.DeepEqual(ops, wantOps) {
		t.Errorf("operators=%v, want %v", ops, wantOps)
	}
	wantComps := map[string]bool{"certManager": true, "argoWorkflows": true, "argocd": true}
	if !reflect.DeepEqual(comps, wantComps) {
		t.Errorf("components=%v, want %v", comps, wantComps)
	}
}

func TestParseImageVersions(t *testing.T) {
	wls := []workload{
		{Images: []string{"quay.io/otomi/core:v4.14.1"}},
		{Images: []string{"goharbor/harbor-core:v2.9.0"}},
		{Images: []string{"grafana/loki:2.9.2", "grafana/grafana:10.1.0"}},
		{Images: []string{"registry.example.com:5000/app"}}, // registry port, no tag → ignored
	}
	apl, versions := parseImageVersions(wls)
	if apl != "v4.14.1" {
		t.Errorf("aplVersion=%q, want v4.14.1", apl)
	}
	if versions["harbor"] != "v2.9.0" || versions["loki"] != "2.9.2" || versions["grafana"] != "10.1.0" {
		t.Errorf("versions=%v", versions)
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"nginx:1.25":                       "1.25",
		"grafana/loki:2.9.2":               "2.9.2",
		"registry.example.com:5000/app":    "",
		"registry.example.com:5000/app:v1": "v1",
		"app@sha256:abc":                   "",
		"plainnotag":                       "",
	}
	for in, want := range cases {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestParseClusterIssuers(t *testing.T) {
	js := `{"items":[
		{"spec":{"acme":{"email":"ops@example.com","solvers":[{"dns01":{"webhook":{}}}]}}},
		{"spec":{"acme":{"email":"ignored@example.com","solvers":[{"http01":{}}]}}}
	]}`
	email, solvers := parseClusterIssuers(js)
	if email != "ops@example.com" {
		t.Errorf("email=%q", email)
	}
	if !reflect.DeepEqual(solvers, []string{"dns01", "http01"}) {
		t.Errorf("solvers=%v", solvers)
	}
}

func TestParseResourceQuotas(t *testing.T) {
	js := `{"items":[
		{"metadata":{"namespace":"team-gsap"},"spec":{"hard":{"limits.cpu":"8","limits.memory":"16Gi","pods":"50"}}}
	]}`
	got := parseResourceQuotas(js)
	q := got["team-gsap"]
	if q["limits.cpu"] != "8" || q["limits.memory"] != "16Gi" {
		t.Errorf("quota=%v", q)
	}
	if _, ok := q["pods"]; ok {
		t.Error("non cpu/memory keys should be dropped")
	}
}

func TestMergeHostSourcesAndAllHostValues(t *testing.T) {
	merged := mergeHostSources(
		map[string][]string{"team-a": {"a.example.com"}},
		map[string][]string{"team-a": {"b.example.com"}, "team-b": {"c.example.com"}},
	)
	if len(merged["team-a"]) != 2 {
		t.Errorf("team-a hosts=%v", merged["team-a"])
	}
	all := allHostValues(merged)
	want := []string{"a.example.com", "b.example.com", "c.example.com"}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("allHostValues=%v, want %v", all, want)
	}
}
