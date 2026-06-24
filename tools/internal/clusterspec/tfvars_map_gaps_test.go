package clusterspec

// Coverage for the optional-field branches of the spec→tfvars mappers and the
// YAML scalar setters: a fully-populated Cluster emits every optional key, a
// minimal one emits none, and the setters no-op on a nil/absent node.

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func assignKeys(as []Assign) map[string]string {
	m := make(map[string]string, len(as))
	for _, a := range as {
		m[a.Key] = a.Val
	}
	return m
}

func fullCluster() Cluster {
	tru, fls := true, false
	var c Cluster
	c.ClusterLabel = "lab"
	c.Region = "us-ord"
	c.K8sVersion = "v1.33.6+lke7"
	c.Tags = []string{"platform"}
	c.NodePool.Type = "g8-dedicated-8-4"
	c.NodePool.Count = 5
	c.NodePool.AutoscalerEnabled = &tru
	c.ControlPlane.HighAvailability = &tru
	c.ControlPlane.AuditLogsEnabled = &fls
	c.APIServerAllowCIDRs.IPv4 = []string{"1.2.3.4/32"}
	c.APIServerAllowCIDRs.IPv6 = []string{"::1/128"}
	c.Network.SubnetCIDR = "10.0.0.0/14"
	c.Network.VPC = "shared-ord"
	c.HA.Role = "active"
	c.HA.Group = "pair-1"
	c.PromotionRank = 2
	c.Bootstrap.Name = "platform-lab"
	c.Bootstrap.DomainSuffix = "lab.example.com"
	c.Bootstrap.AplChartVersion = "1.2.3"
	c.Bootstrap.AplValues.RepoURL = "https://git/x"
	c.Bootstrap.AplValues.Revision = "main"
	c.Bootstrap.AplValues.Username = "ci"
	c.Bootstrap.AppsRepoRevision = "abc123"
	c.ObjectStorage.Cluster = "us-ord-1"
	c.ObjectStorage.KeyRotationDays = 30
	return c
}

func TestClusterTFVars_AllOptionalsEmitted(t *testing.T) {
	keys := assignKeys(ClusterTFVars(fullCluster()))
	for _, k := range []string{
		"cluster_label", "region", "k8s_version", "tags", "node_type", "node_count",
		"autoscaler_enabled", "control_plane_high_availability", "control_plane_audit_logs_enabled",
		"github_runner_ipv4_cidrs", "github_runner_ipv6_cidrs", "vpc_subnet_cidr", "vpc_network",
		"ha_role", "ha_group",
	} {
		if _, ok := keys[k]; !ok {
			t.Errorf("ClusterTFVars(full) missing %q", k)
		}
	}
}

func TestClusterTFVars_MinimalOmitsOptionals(t *testing.T) {
	var c Cluster
	c.ClusterLabel, c.Region, c.K8sVersion = "l", "r", "v"
	c.NodePool.Type, c.NodePool.Count = "t", 1
	keys := assignKeys(ClusterTFVars(c))
	for _, k := range []string{"tags", "autoscaler_enabled", "vpc_subnet_cidr", "vpc_network", "ha_role", "ha_group"} {
		if _, ok := keys[k]; ok {
			t.Errorf("ClusterTFVars(minimal) should omit %q", k)
		}
	}
	// node_count is always emitted, even at the zero-ish default.
	if _, ok := keys["node_count"]; !ok {
		t.Error("node_count should always be emitted")
	}
}

func TestBootstrapTFVars_Optionals(t *testing.T) {
	full := assignKeys(BootstrapTFVars("prod", fullCluster()))
	for _, k := range []string{
		"deployment", "apl_values_env", "cluster_name", "cluster_domain",
		"apl_chart_version", "apl_values_repo_url", "apl_values_repo_revision",
		"apl_values_repo_username", "apps_repo_revision",
	} {
		if _, ok := full[k]; !ok {
			t.Errorf("BootstrapTFVars(full) missing %q", k)
		}
	}
	if full["deployment"] != `"prod"` {
		t.Errorf("deployment = %q, want \"prod\"", full["deployment"])
	}

	var c Cluster
	c.Bootstrap.Name, c.Bootstrap.DomainSuffix = "n", "d"
	min := assignKeys(BootstrapTFVars("dev", c))
	for _, k := range []string{"apl_chart_version", "apl_values_repo_url", "apps_repo_revision"} {
		if _, ok := min[k]; ok {
			t.Errorf("BootstrapTFVars(minimal) should omit %q", k)
		}
	}
}

func TestObjectStorageTFVars(t *testing.T) {
	full := assignKeys(ObjectStorageTFVars("prod", fullCluster()))
	for _, k := range []string{"region_suffix", "obj_cluster", "obj_key_rotation_days"} {
		if _, ok := full[k]; !ok {
			t.Errorf("ObjectStorageTFVars(full) missing %q", k)
		}
	}
	// Minimal: only region_suffix; optional cluster/rotation omitted.
	min := assignKeys(ObjectStorageTFVars("dev", Cluster{}))
	if _, ok := min["region_suffix"]; !ok {
		t.Error("region_suffix should always be emitted")
	}
	if _, ok := min["obj_cluster"]; ok {
		t.Error("obj_cluster should be omitted when unset")
	}
	if _, ok := min["obj_key_rotation_days"]; ok {
		t.Error("obj_key_rotation_days should be omitted when zero")
	}
}

func TestScalarSettersNilNoOp(t *testing.T) {
	// nil node → no-op, no panic.
	setBool(nil, true)
	setInt(nil, 7)

	// A real scalar node gets overwritten with the typed literal.
	n := &yaml.Node{Kind: yaml.ScalarNode, Value: "old"}
	setBool(n, true)
	if n.Tag != "!!bool" || n.Value != "true" {
		t.Errorf("setBool = (%q,%q)", n.Tag, n.Value)
	}
	setInt(n, 42)
	if n.Tag != "!!int" || n.Value != "42" {
		t.Errorf("setInt = (%q,%q)", n.Tag, n.Value)
	}
}

func TestMapValue(t *testing.T) {
	if mapValue(nil, "k") != nil {
		t.Error("mapValue(nil) should be nil")
	}
	scalar := &yaml.Node{Kind: yaml.ScalarNode, Value: "x"}
	if mapValue(scalar, "k") != nil {
		t.Error("mapValue(non-mapping) should be nil")
	}
	m := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "k"},
		{Kind: yaml.ScalarNode, Value: "v"},
	}}
	if got := mapValue(m, "k"); got == nil || got.Value != "v" {
		t.Errorf("mapValue(k) = %v", got)
	}
	if mapValue(m, "absent") != nil {
		t.Error("mapValue(absent) should be nil")
	}
}
