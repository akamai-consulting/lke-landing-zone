package linode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLinodeIDFromProviderID(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"linode://12345", 12345, true},
		{"linode://", 0, false},
		{"linode://12a45", 0, false},
		{"aws:///us-east-1a/i-abc", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := LinodeIDFromProviderID(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("LinodeIDFromProviderID(%q) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestLKEClusterIDFromNodeName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"lke393244-59879-0a1b2c3d", "393244"},
		{"lke1-2-x", "1"},
		{"lke-59879-0a1b2c3d", ""}, // no digits before the dash
		{"lke393244", ""},          // no pool segment — not a node name
		{"node-1", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := LKEClusterIDFromNodeName(tc.in); got != tc.want {
			t.Errorf("LKEClusterIDFromNodeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestVPCInterface(t *testing.T) {
	configs := []map[string]any{
		{"id": json.Number("1"), "interfaces": []any{
			map[string]any{"purpose": "public"},
		}},
		{"id": json.Number("2"), "interfaces": []any{
			map[string]any{"purpose": "vpc", "vpc_id": json.Number("77"), "subnet_id": json.Number("88")},
		}},
	}
	vpcID, subnetID, ok := VPCInterface(configs)
	if !ok || vpcID != 77 || subnetID != 88 {
		t.Errorf("VPCInterface = (%d,%d,%v), want (77,88,true)", vpcID, subnetID, ok)
	}

	if _, _, ok := VPCInterface([]map[string]any{{"interfaces": nil}}); ok {
		t.Error("nil interfaces should return ok=false")
	}
	if _, _, ok := VPCInterface(nil); ok {
		t.Error("no configs should return ok=false")
	}
}

func TestSubnetIPv4(t *testing.T) {
	subnets := []map[string]any{
		{"id": json.Number("5"), "ipv4": "10.0.0.0/24"},
		{"id": json.Number("88"), "ipv4": "10.1.0.0/24"},
	}
	if cidr, ok := SubnetIPv4(subnets, 88); !ok || cidr != "10.1.0.0/24" {
		t.Errorf("SubnetIPv4 = (%q,%v), want (10.1.0.0/24,true)", cidr, ok)
	}
	if _, ok := SubnetIPv4(subnets, 999); ok {
		t.Error("unknown subnet id should return ok=false")
	}
}

func TestInstanceLookupsHitExpectedPaths(t *testing.T) {
	// One handler serving all three endpoints so each method is exercised
	// against the URL it is supposed to build.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v4/linode/instances/42/firewalls":
			w.Write([]byte(`{"data":[{"id":7,"label":"primary-nodes-fw"}],"pages":1}`))
		case "/v4/linode/instances/42/configs":
			w.Write([]byte(`{"data":[{"id":1,"interfaces":[{"purpose":"vpc","vpc_id":77,"subnet_id":88}]}],"pages":1}`))
		case "/v4/linode/instances/42":
			w.Write([]byte(`{"id":42,"lke_cluster_id":393244}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient("tok", 5*time.Second)
	c.base = srv.URL
	ctx := context.Background()

	fws, err := c.InstanceFirewalls(ctx, 42)
	if err != nil || len(fws) != 1 || mUint(fws[0], "id") != 7 {
		t.Errorf("InstanceFirewalls = (%v,%v), want one firewall id 7", fws, err)
	}

	cfgs, err := c.InstanceConfigs(ctx, 42)
	if err != nil {
		t.Fatalf("InstanceConfigs: %v", err)
	}
	if vpcID, subnetID, ok := VPCInterface(cfgs); !ok || vpcID != 77 || subnetID != 88 {
		t.Errorf("VPCInterface over InstanceConfigs = (%d,%d,%v), want (77,88,true)", vpcID, subnetID, ok)
	}

	id, err := c.InstanceLKEClusterID(ctx, 42)
	if err != nil || id != 393244 {
		t.Errorf("InstanceLKEClusterID = (%d,%v), want 393244", id, err)
	}
}

func TestInstanceLKEClusterIDNullAndError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v4/linode/instances/1":
			w.Write([]byte(`{"id":1,"lke_cluster_id":null}`)) // non-LKE instance
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()

	c := NewClient("tok", 5*time.Second)
	c.base = srv.URL

	if id, err := c.InstanceLKEClusterID(context.Background(), 1); err != nil || id != 0 {
		t.Errorf("null lke_cluster_id = (%d,%v), want (0,nil)", id, err)
	}
	if _, err := c.InstanceLKEClusterID(context.Background(), 2); err == nil {
		t.Error("non-2xx should return an error")
	}
}
