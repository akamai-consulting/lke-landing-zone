package terraform

import (
	"encoding/json"
	"testing"
)

func TestParseTFVars(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantCluster string
		wantPool    string
	}{
		{"both set", "cluster_label = \"primary-llz\"\nnode_pool_label = \"obs\"\n", "primary-llz", "obs"},
		{"pool defaulted", "cluster_label = \"primary-llz\"\n", "primary-llz", DefaultNodePoolLabel},
		{"extra whitespace", "cluster_label    =   \"c1\"  \n", "c1", DefaultNodePoolLabel},
		{"first wins", "cluster_label = \"first\"\ncluster_label = \"second\"\n", "first", DefaultNodePoolLabel},
		{"ignores other keys", "region = \"us-ord\"\ncluster_label = \"c2\"\n", "c2", DefaultNodePoolLabel},
		{"empty", "", "", DefaultNodePoolLabel},
		{"no key-prefix false match", "my_cluster_label = \"nope\"\n", "", DefaultNodePoolLabel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := ParseTFVars(c.in)
			if v.ClusterLabel != c.wantCluster {
				t.Errorf("ClusterLabel = %q, want %q", v.ClusterLabel, c.wantCluster)
			}
			if v.NodePoolLabel != c.wantPool {
				t.Errorf("NodePoolLabel = %q, want %q", v.NodePoolLabel, c.wantPool)
			}
		})
	}
}

func TestParseTFVarsNodePoolSizing(t *testing.T) {
	v := ParseTFVars("node_type = \"g6-standard-4\"\nnode_count = 3\n")
	if v.NodeType != "g6-standard-4" {
		t.Errorf("NodeType = %q, want g6-standard-4", v.NodeType)
	}
	if v.NodeCount != 3 {
		t.Errorf("NodeCount = %d, want 3", v.NodeCount)
	}
	// Absent node sizing => zero value (preflight treats node_count 0 as unknown).
	if z := ParseTFVars("cluster_label = \"x\"\n"); z.NodeType != "" || z.NodeCount != 0 {
		t.Errorf("absent sizing = (%q,%d), want (\"\",0)", z.NodeType, z.NodeCount)
	}
}

func TestDeriveLabels(t *testing.T) {
	got := DeriveLabels(TFVars{ClusterLabel: "primary", NodePoolLabel: "obs"})
	want := Labels{Cluster: "primary", NodePool: "obs", VPC: "primary-vpc", Subnet: "primary-nodes", Firewall: "primary-nodes"}
	if got != want {
		t.Errorf("DeriveLabels = %+v, want %+v", got, want)
	}
}

func TestResolveFirewallLabel(t *testing.T) {
	// Short label: plain <cluster>-nodes (matches llz-cluster/main.tf).
	if got := ResolveFirewallLabel(TFVars{ClusterLabel: "primary"}); got != "primary-nodes" {
		t.Errorf("short = %q, want primary-nodes", got)
	}
	// Override wins, used verbatim (the module does not truncate it).
	if got := ResolveFirewallLabel(TFVars{ClusterLabel: "primary", FirewallLabel: "my-fw"}); got != "my-fw" {
		t.Errorf("override = %q, want my-fw", got)
	}
	// Long label: substr(cluster,0,26)+"-nodes" — the module-correct truncation,
	// NOT the retired import script's (cluster+"-nodes")[:32].
	cluster := "abcdefghijklmnopqrstuvwxyz0123456789" // 36 chars
	got := ResolveFirewallLabel(TFVars{ClusterLabel: cluster})
	if want := cluster[:26] + "-nodes"; got != want {
		t.Errorf("long = %q, want %q", got, want)
	}
}

func TestSelectNodePoolID(t *testing.T) {
	// ids as json.Number — the shape the Linode client produces (UseNumber).
	pools := []map[string]any{
		{"id": json.Number("11"), "label": "other", "tags": []any{"x"}},
		{"id": json.Number("22"), "label": "", "tags": []any{"observability-pool"}},
		{"id": json.Number("33"), "label": "observability-pool"},
	}
	// Exact label match wins over a tag match on an earlier pool.
	if id, ok := SelectNodePoolID(pools, "observability-pool"); !ok || id != 33 {
		t.Errorf("label match = (%d,%v), want (33,true)", id, ok)
	}
	// Falls back to a tag match when no label matches.
	if id, ok := SelectNodePoolID(pools, "x"); !ok || id != 11 {
		t.Errorf("tag fallback = (%d,%v), want (11,true)", id, ok)
	}
	// Tag-only pool found by its tag.
	tagOnly := []map[string]any{{"id": json.Number("22"), "tags": []any{"observability-pool"}}}
	if id, ok := SelectNodePoolID(tagOnly, "observability-pool"); !ok || id != 22 {
		t.Errorf("tag-only match = (%d,%v), want (22,true)", id, ok)
	}
	if _, ok := SelectNodePoolID(pools, "absent"); ok {
		t.Error("no match should return ok=false")
	}
}

func TestParseStateID(t *testing.T) {
	out := `# module.cluster.linode_vpc.this:
resource "linode_vpc" "this" {
    description = ""
    id          = "12345"
    label       = "primary-vpc"
}`
	if got := ParseStateID(out); got != "12345" {
		t.Errorf("ParseStateID = %q, want 12345", got)
	}
	if got := ParseStateID("no id here"); got != "" {
		t.Errorf("ParseStateID(no id) = %q, want empty", got)
	}
	// A later `something_id =` line must not be mistaken for the id attribute.
	out2 := "    vpc_id = \"999\"\n    id     = \"42\"\n"
	if got := ParseStateID(out2); got != "42" {
		t.Errorf("ParseStateID = %q, want 42", got)
	}
}

func TestKubeconfigContent(t *testing.T) {
	// "apiVersion: v1" base64 -> decodes to real content.
	content, stub := KubeconfigContent("YXBpVmVyc2lvbjogdjE=")
	if stub || string(content) != "apiVersion: v1" {
		t.Errorf("decode = (%q, stub=%v), want real content", content, stub)
	}
	// Empty -> stub.
	if c, stub := KubeconfigContent(""); !stub || string(c) != StubKubeconfig {
		t.Errorf("empty should yield stub, got (%q, %v)", c, stub)
	}
	// Garbage -> stub (not an error).
	if _, stub := KubeconfigContent("!!!not base64!!!"); !stub {
		t.Error("undecodable input should fall back to stub")
	}
}
