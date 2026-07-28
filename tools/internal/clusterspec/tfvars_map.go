package clusterspec

import (
	"sort"
	"strconv"
	"strings"
)

// tfvars_map.go is the pure spec→tfvars field mapping. Each function returns the
// ordered assignments for one Terraform root; the renderer (cmd/llz/render.go)
// applies them onto the root's terraform.tfvars.example with setHCLField
// (set-or-append). Keeping the mapping here — returning already-formatted HCL
// right-hand sides — makes it unit-testable without touching the filesystem,
// and keeps the cmd/llz renderer a thin apply loop.
//
// An assignment is emitted only when the spec PROVIDES the value: required
// fields always, optional strings when non-empty, optional bools (*bool) when
// non-nil, list fields when non-nil (an explicit empty list renders as []), and
// counts/ranks when positive. Omitted optional fields leave the example default
// untouched — same contract as `llz env add`.

// Assign is one `key = <hcl>` tfvars assignment; Val is the formatted RHS.
type Assign struct {
	Key string
	Val string
}

// ClusterTFVars maps spec.cluster onto cluster/<env>.tfvars.
func ClusterTFVars(c Cluster) []Assign {
	var a []Assign
	add := func(k, v string) { a = append(a, Assign{k, v}) }

	add("cluster_label", hclStr(c.ClusterLabel))
	add("region", hclStr(c.Region))
	add("k8s_version", hclStr(c.K8sVersion))
	if c.Tags != nil {
		add("tags", hclStrList(c.Tags))
	}
	add("node_type", hclStr(c.NodePool.Type))
	add("node_count", strconv.Itoa(c.NodePool.Count))
	// Managed App Platform: apl_enabled=true makes Linode install+manage apl-core
	// and provision the akamai-apl.net domain. Always emitted (false = self-install,
	// the default). See Bootstrap.ManagedAppPlatform / ADR 0005.
	add("apl_enabled", hclBool(c.Bootstrap.ManagedAppPlatform))
	if c.NodePool.AutoscalerEnabled != nil {
		add("autoscaler_enabled", hclBool(*c.NodePool.AutoscalerEnabled))
	}
	if c.NodePool.AutoscalerMin != nil {
		add("autoscaler_min", strconv.Itoa(*c.NodePool.AutoscalerMin))
	}
	if c.NodePool.AutoscalerMax != nil {
		add("autoscaler_max", strconv.Itoa(*c.NodePool.AutoscalerMax))
	}
	if c.ControlPlane.HighAvailability != nil {
		add("control_plane_high_availability", hclBool(*c.ControlPlane.HighAvailability))
	}
	if c.ControlPlane.AuditLogsEnabled != nil {
		add("control_plane_audit_logs_enabled", hclBool(*c.ControlPlane.AuditLogsEnabled))
	}
	if c.APIServerAllowCIDRs.IPv4 != nil {
		add("github_runner_ipv4_cidrs", hclStrList(c.APIServerAllowCIDRs.IPv4))
	}
	if c.APIServerAllowCIDRs.IPv6 != nil {
		add("github_runner_ipv6_cidrs", hclStrList(c.APIServerAllowCIDRs.IPv6))
	}
	if c.Network.SubnetCIDR != "" {
		add("vpc_subnet_cidr", hclStr(c.Network.SubnetCIDR))
	}
	// A shared VPC: the cluster attaches to the named VPC (from spec.networks)
	// instead of creating its own. Empty → dedicated VPC (the default).
	if c.Network.VPC != "" {
		add("vpc_network", hclStr(c.Network.VPC))
	}
	if c.HA.Role != "" {
		add("ha_role", hclStr(c.HA.Role))
	}
	if c.HA.Group != "" {
		add("ha_group", hclStr(c.HA.Group))
	}
	return a
}

// NOTE — the former BootstrapTFVars (spec.cluster.bootstrap →
// cluster-bootstrap/<env>.tfvars) was removed with the cluster-bootstrap TF
// workspace: the in-cluster bootstrap now runs natively via `llz ci
// bootstrap-cluster`, which reads the apl-core chart version + apps-repo-revision
// straight from the spec (no tfvars hop). The spec fields it consumed
// (spec.cluster.bootstrap.*) remain on the type.

// NetworkTFVars maps one spec.networks entry onto vpc/<name>.tfvars — the shared-VPC
// root (one apply per network, state key vpc/<name>) that provisions a single
// region-scoped linode_vpc labelled by the network name. The cluster root attaches
// to it by that label (var.vpc_network).
func NetworkTFVars(name string, v VPC) []Assign {
	return []Assign{
		{"vpc_label", hclStr(name)},
		{"region", hclStr(v.Region)},
	}
}

// DatabasesTFVars maps spec.cluster.databases onto databases/<env>.tfvars. Like
// object storage, region_suffix is always the env name. The clusters themselves
// render as ONE assignment — `databases = { "<name>" = { … } }` — because the
// root fans out with `for_each = var.databases`; see hclDatabases.
//
// With no clusters declared, only region_suffix is emitted, which leaves the
// example's `databases = {}` in place: the root applies and provisions nothing.
// That is the 0 in 0-n, and it is why `databases` needs no enabled flag.
func DatabasesTFVars(env string, c Cluster) []Assign {
	a := []Assign{{"region_suffix", hclStr(env)}}
	if len(c.Databases) > 0 {
		a = append(a, Assign{"databases", hclDatabases(c.Databases)})
	}
	return a
}

// hclDatabases formats spec.cluster.databases as an HCL map of objects matching
// the root's `map(object({…}))`. Keys are sorted so a re-render of an unchanged
// spec is byte-identical — Go map iteration is randomized, and `llz render
// --check` compares bytes, so unsorted output would report drift on every run.
//
// Required attributes (region/vpc_id/subnet_id) are always written, including
// their zero values: the root's own validation rejects vpc_id 0 with a message
// naming the fix, which beats a missing-attribute error naming a tfvars file the
// operator never wrote. The optional ones are omitted unless the spec sets them,
// so the root's `optional(…, default)` stays the single source of the defaults.
//
// Output is deliberately un-aligned; renderTfvars pipes the result through
// `tofu fmt`, which owns the `=` alignment the instance's fmt-check enforces.
func hclDatabases(dbs Databases) string {
	names := make([]string, 0, len(dbs))
	for n := range dbs {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("{\n")
	for _, n := range names {
		d := dbs[n]
		b.WriteString(hclStr(n) + " = {\n")
		b.WriteString("region = " + hclStr(d.Region) + "\n")
		b.WriteString("vpc_id = " + strconv.Itoa(d.VPCID) + "\n")
		b.WriteString("subnet_id = " + strconv.Itoa(d.SubnetID) + "\n")
		if d.EngineVersion != "" {
			b.WriteString("engine_version = " + hclStr(d.EngineVersion) + "\n")
		}
		if d.Type != "" {
			b.WriteString("db_type = " + hclStr(d.Type) + "\n")
		}
		if d.ClusterSize != 0 {
			b.WriteString("cluster_size = " + strconv.Itoa(d.ClusterSize) + "\n")
		}
		b.WriteString("}\n")
	}
	b.WriteString("}")
	return b.String()
}

// ObjectStorageTFVars maps spec.cluster.objectStorage onto object-storage/<env>.tfvars.
func ObjectStorageTFVars(env string, c Cluster) []Assign {
	var a []Assign
	add := func(k, v string) { a = append(a, Assign{k, v}) }

	add("region_suffix", hclStr(env))
	if c.ObjectStorage.Cluster != "" {
		add("obj_cluster", hclStr(c.ObjectStorage.Cluster))
	}
	// spec.cluster.objectStorage.keyRotationDays is NO LONGER emitted: the
	// obj_key_rotation_days variable was removed with the TF-managed keys (the
	// in-cluster linodeCredRotator owns rotation). The spec field is accepted
	// but ignored so existing specs keep strict-parsing.
	return a
}

// ── HCL formatting (kept local to the mapping; trivial + stable) ─────────────

func hclStr(s string) string { return `"` + s + `"` }

func hclBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func hclStrList(items []string) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		if it = strings.TrimSpace(it); it != "" {
			parts = append(parts, hclStr(it))
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
