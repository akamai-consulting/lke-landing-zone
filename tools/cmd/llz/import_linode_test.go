package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// decodeLinode mimics the client's UseNumber decoding so numeric fields arrive as
// json.Number (the shape the extractors must tolerate).
func decodeLinode(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func decodeLinodeList(t *testing.T, s string) []map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var l []map[string]any
	if err := dec.Decode(&l); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestLkeClusterIDFromContext(t *testing.T) {
	cases := map[string]uint64{
		"lke579582-ctx":    579582,
		"lke12345":         12345,
		"my-prod-cluster":  0,
		"admin@lke999-ctx": 999,
	}
	for in, want := range cases {
		if got := lkeClusterIDFromContext(in); got != want {
			t.Errorf("lkeClusterIDFromContext(%q)=%d, want %d", in, got, want)
		}
	}
}

func TestLkeClusterInfo(t *testing.T) {
	m := decodeLinode(t, `{"id":579582,"label":"gsap-prod","region":"us-ord","k8s_version":"1.33","control_plane":{"high_availability":true},"tags":["env:prod"]}`)
	got := lkeClusterInfo(m)
	if got.Label != "gsap-prod" || got.Region != "us-ord" || got.K8sVersion != "1.33" || !got.ControlPlaneHA {
		t.Errorf("clusterInfo=%+v", got)
	}
	if !reflect.DeepEqual(got.Tags, []string{"env:prod"}) {
		t.Errorf("tags=%v", got.Tags)
	}
}

func TestLkeNodePools(t *testing.T) {
	pools := decodeLinodeList(t, `[
		{"id":202,"type":"g6-dedicated-8","count":2,"autoscaler":{"enabled":true,"min":2,"max":6}},
		{"id":101,"type":"g6-standard-4","count":3}
	]`)
	got := lkeNodePools(pools)
	if len(got) != 2 || got[0].ID != 101 || got[1].ID != 202 { // sorted by id
		t.Fatalf("pools=%+v", got)
	}
	if got[1].Type != "g6-dedicated-8" || !got[1].AutoscalerEnabled || got[1].Min != 2 || got[1].Max != 6 {
		t.Errorf("autoscaling pool=%+v", got[1])
	}
	if got[0].AutoscalerEnabled {
		t.Error("pool 101 should not be autoscaling")
	}
}

func TestMatchClusterVPC(t *testing.T) {
	vpcs := decodeLinodeList(t, `[
		{"id":1,"label":"other"},
		{"id":2,"label":"gsap-prod-vpc"}
	]`)
	v, ok := matchClusterVPC(vpcs, "gsap-prod")
	if !ok || mapUint(v, "id") != 2 {
		t.Errorf("matched=%v ok=%v", v, ok)
	}
	if _, ok := matchClusterVPC(vpcs, ""); ok {
		t.Error("empty label should not match")
	}
}

func TestVPCSubnetCIDRs(t *testing.T) {
	subnets := decodeLinodeList(t, `[{"id":1,"ipv4":"10.0.0.0/24"},{"id":2,"ipv4":"10.0.1.0/24"},{"id":3}]`)
	got := vpcSubnetCIDRs(subnets)
	if !reflect.DeepEqual(got, []string{"10.0.0.0/24", "10.0.1.0/24"}) {
		t.Errorf("cidrs=%v", got)
	}
}

func TestClusterFirewallsAndInfo(t *testing.T) {
	fws := decodeLinodeList(t, `[
		{"id":10,"label":"platform-nodes-fw","rules":{"inbound_policy":"DROP","inbound":[
			{"action":"ACCEPT","addresses":{"ipv4":["1.2.3.4/32"],"ipv6":["::1/128"]}},
			{"action":"ACCEPT","addresses":{"ipv4":["5.6.7.8/32"]}}
		]}},
		{"id":20,"label":"unrelated-fw"}
	]`)
	got := clusterFirewalls(fws, "gsap-prod", 579582)
	if len(got) != 1 || got[0].Label != "platform-nodes-fw" {
		t.Fatalf("firewalls=%+v", got)
	}
	if got[0].InboundPolicy != "DROP" {
		t.Errorf("policy=%q", got[0].InboundPolicy)
	}
	want := []string{"1.2.3.4/32", "5.6.7.8/32", "::1/128"}
	if !reflect.DeepEqual(got[0].InboundCIDRs, want) {
		t.Errorf("cidrs=%v, want %v", got[0].InboundCIDRs, want)
	}
}

func TestClusterFirewallsMatchByLabelAndTag(t *testing.T) {
	fws := decodeLinodeList(t, `[
		{"id":1,"label":"gsap-prod-nodes"},
		{"id":2,"label":"x","tags":["lke579582"]},
		{"id":3,"label":"lke-579582"},
		{"id":4,"label":"nope"}
	]`)
	got := clusterFirewalls(fws, "gsap-prod", 579582)
	if len(got) != 3 { // label-contains + id-tag + lke-<id> convention
		t.Errorf("got %d, want 3: %+v", len(got), got)
	}
}

func TestObjectStorageBuckets(t *testing.T) {
	buckets := decodeLinodeList(t, `[
		{"label":"gsap-loki","region":"us-ord","objects":1200},
		{"label":"gsap-harbor","cluster":"us-ord-1","objects":50}
	]`)
	got := objectStorageBuckets(buckets)
	if len(got) != 2 || got[0].Label != "gsap-harbor" { // sorted by label
		t.Fatalf("buckets=%+v", got)
	}
	if got[0].Region != "us-ord-1" { // falls back to cluster field
		t.Errorf("region fallback=%q", got[0].Region)
	}
	if got[1].Region != "us-ord" || got[1].Objects != 1200 {
		t.Errorf("bucket=%+v", got[1])
	}
}

func TestCountClusterNodeBalancers(t *testing.T) {
	nbs := decodeLinodeList(t, `[
		{"id":1,"lke_cluster":{"id":579582}},
		{"id":2,"lke_cluster":{"id":579582}},
		{"id":3,"lke_cluster":{"id":111}}
	]`)
	if n := countClusterNodeBalancers(nbs, 579582); n != 2 {
		t.Errorf("got %d, want 2", n)
	}
}

func TestMapHelpersJSONNumber(t *testing.T) {
	m := decodeLinode(t, `{"n":42,"b":true,"s":"x","f":3.0}`)
	if mapInt(m, "n") != 42 || mapUint(m, "n") != 42 {
		t.Errorf("int/uint from json.Number failed: %v", m["n"])
	}
	if !mapBool(m, "b") || mapString(m, "s") != "x" {
		t.Error("bool/string helpers failed")
	}
}
