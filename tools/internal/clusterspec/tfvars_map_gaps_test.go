package clusterspec

// Coverage for the optional-field branches of the spec→tfvars mappers: a
// fully-populated Cluster emits every optional key, a minimal one emits none.

import (
	"strings"
	"testing"
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
	amin, amax := 2, 9
	c.NodePool.AutoscalerMin = &amin
	c.NodePool.AutoscalerMax = &amax
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
		"autoscaler_enabled", "autoscaler_min", "autoscaler_max",
		"control_plane_high_availability", "control_plane_audit_logs_enabled",
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
	for _, k := range []string{"tags", "autoscaler_enabled", "autoscaler_min", "autoscaler_max", "vpc_subnet_cidr", "vpc_network", "ha_role", "ha_group"} {
		if _, ok := keys[k]; ok {
			t.Errorf("ClusterTFVars(minimal) should omit %q", k)
		}
	}
	// node_count is always emitted, even at the zero-ish default.
	if _, ok := keys["node_count"]; !ok {
		t.Error("node_count should always be emitted")
	}
}

// apl_enabled (managed App Platform, ADR 0005) is ALWAYS emitted — false by
// default (self-install), true when spec.cluster.bootstrap.managedAppPlatform.
func TestClusterTFVars_APLEnabled(t *testing.T) {
	var c Cluster
	c.ClusterLabel, c.Region, c.K8sVersion = "l", "r", "v"
	c.NodePool.Type, c.NodePool.Count = "t", 1
	find := func(cl Cluster) string {
		for _, a := range ClusterTFVars(cl) {
			if a.Key == "apl_enabled" {
				return a.Val
			}
		}
		return "<absent>"
	}
	if got := find(c); got != "false" {
		t.Errorf("default apl_enabled = %q, want false (self-install)", got)
	}
	c.Bootstrap.ManagedAppPlatform = true
	if got := find(c); got != "true" {
		t.Errorf("managedAppPlatform → apl_enabled = %q, want true", got)
	}
}

func TestObjectStorageTFVars(t *testing.T) {
	full := assignKeys(ObjectStorageTFVars("prod", fullCluster()))
	for _, k := range []string{"region_suffix", "obj_cluster"} {
		if _, ok := full[k]; !ok {
			t.Errorf("ObjectStorageTFVars(full) missing %q", k)
		}
	}
	// obj_key_rotation_days is NEVER emitted (the TF variable was removed with
	// the TF-managed keys; the in-cluster rotator owns rotation) — even when the
	// deprecated spec field is set.
	if _, ok := full["obj_key_rotation_days"]; ok {
		t.Error("obj_key_rotation_days must not be emitted (variable removed; rotator owns rotation)")
	}
	// Minimal: only region_suffix; optional cluster omitted.
	min := assignKeys(ObjectStorageTFVars("dev", Cluster{}))
	if _, ok := min["region_suffix"]; !ok {
		t.Error("region_suffix should always be emitted")
	}
	if _, ok := min["obj_cluster"]; ok {
		t.Error("obj_cluster should be omitted when unset")
	}
}

// TestDatabasesTFVars pins the three shapes that matter for an OPT-IN, 0-n root:
// zero clusters, one, and several.
//
// ZERO is the common case and the load-bearing one: `llz render` writes a
// databases/<env>.tfvars for every env whether or not the instance wants a
// database, so the stub must carry region_suffix and NOTHING else. Omitting the
// `databases` assignment leaves the example's `databases = {}` in place, and the
// root then applies cleanly and provisions nothing. Emitting an empty-but-present
// entry instead would hand the root a syntactically valid tfvars naming VPC 0 —
// an apply against the wrong thing rather than a clean no-op.
func TestDatabasesTFVars(t *testing.T) {
	// Zero clusters — the stub: region_suffix only, no `databases` key at all.
	stub := assignKeys(DatabasesTFVars("dev", Cluster{}))
	if got := stub["region_suffix"]; got != `"dev"` {
		t.Errorf("region_suffix must always be emitted, got %q", got)
	}
	if _, ok := stub["databases"]; ok {
		t.Error("`databases` must be omitted when the spec declares no clusters, so the example's `databases = {}` stands")
	}

	// One cluster, fully specified.
	var c Cluster
	c.Databases = Databases{"shared": {
		Region: "us-ord", VPCID: 575244, SubnetID: 12345,
		EngineVersion: "16", Type: "g6-dedicated-2", ClusterSize: 2,
	}}
	one := assignKeys(DatabasesTFVars("prod", c))
	if got := one["region_suffix"]; got != `"prod"` {
		t.Errorf("region_suffix = %q, want %q", got, `"prod"`)
	}
	// HCL numbers are unquoted, or the number-typed object attributes reject them.
	want := "{\n" +
		"\"shared\" = {\n" +
		"region = \"us-ord\"\n" +
		"vpc_id = 575244\n" +
		"subnet_id = 12345\n" +
		"engine_version = \"16\"\n" +
		"db_type = \"g6-dedicated-2\"\n" +
		"cluster_size = 2\n" +
		"}\n}"
	if got := one["databases"]; got != want {
		t.Errorf("DatabasesTFVars(one)[\"databases\"] =\n%s\nwant\n%s", got, want)
	}

	// Several clusters: keys SORTED (Go map order is randomized, and `llz render
	// --check` compares bytes — unsorted output would report drift every run), and
	// the unset optional fields omitted so the root's optional() defaults own them.
	c.Databases = Databases{
		"shared":    {Region: "us-ord", VPCID: 575244, SubnetID: 12345},
		"analytics": {Region: "us-ord", VPCID: 575244, SubnetID: 12345, ClusterSize: 1},
	}
	many := assignKeys(DatabasesTFVars("prod", c))["databases"]
	if a, s := strings.Index(many, `"analytics"`), strings.Index(many, `"shared"`); a < 0 || s < 0 || a > s {
		t.Errorf("databases entries must be sorted by name for a byte-stable re-render, got:\n%s", many)
	}
	if strings.Contains(many, "engine_version") || strings.Contains(many, "db_type") {
		t.Errorf("unset optional fields must be omitted so the root's optional() defaults apply, got:\n%s", many)
	}
	if strings.Count(many, "cluster_size") != 1 {
		t.Errorf("cluster_size must be emitted only for the entry that sets it, got:\n%s", many)
	}
}
