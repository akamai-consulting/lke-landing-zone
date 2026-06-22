// Package clusterspec is the declarative front-end for an LKE landing-zone
// instance: a landingzone.yaml (kind: LandingZone) holding the instance identity
// (was .copier-answers.yml) + shared spec.defaults, plus one environments/<env>.yaml
// (kind: ClusterDefinition) per deployment carrying its cluster definition +
// enabled "components" (was the per-env tfvars + the apl-values/<env> manifest
// kustomization). The loader (instance.go) assembles them into one *LandingZone
// the `llz` CLI reconciles into the existing Terraform / Argo / copier config
// (see tools/cmd/llz/render.go).
//
// The types carry json tags and use the apiVersion/kind/metadata/spec shape so
// the same struct tree can graduate to a controller-gen CRD later without a
// rewrite (the "hybrid" decision: CLI-rendered now, CRD-ready). This package is
// pure — no exec, no filesystem beyond Load — so the mapping logic is unit-tested
// the same way internal/terraform is.
package clusterspec

import "sort"

// APIVersion / Kind are the only accepted values for v1alpha1. v1alpha1 signals
// the CLI-rendered, pre-CRD maturity level.
const (
	APIVersion = "llz.akamai-consulting.io/v1alpha1"
	Kind       = "LandingZone"
)

// LandingZone is the assembled root resource: landingzone.yaml's identity +
// defaults, with spec.environments populated from the environments/*.yaml files.
type LandingZone struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

type Metadata struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

type Spec struct {
	// Instance is the one-per-repo identity that copier renders into committed
	// files (was .copier-answers.yml).
	Instance Instance `json:"instance"`
	// Networks declares shared, region-scoped VPCs by name. An environment attaches
	// to one via cluster.network.vpc (omit → its own dedicated VPC). Linode VPCs
	// cannot span regions, so every env on a network must be in the network's region.
	Networks map[string]VPC `json:"networks,omitempty"`
	// Defaults are shared cluster/component settings (landingzone.yaml's
	// `spec.defaults`) inherited by every environment. A per-env value overrides
	// the matching default; an unset env field falls back to the default, then to
	// the built-in default. Empty when every env is fully specified.
	Defaults Defaults `json:"defaults,omitempty"`
	// Environments is keyed by deployment name (== TF workspace key ==
	// apl-values/<env> dir == infra-<env> GitHub Environment). It is the ASSEMBLED
	// model: the loader populates it from the environments/<env>.yaml files (one
	// ClusterDefinition each). Authoring it inline in landingzone.yaml is rejected.
	Environments map[string]Environment `json:"environments,omitempty"`
}

// Defaults is the shared baseline merged into every environment before the
// built-in defaults. It mirrors an Environment's shape (cluster + components) but
// every field is optional — only the keys an author sets are inherited.
type Defaults struct {
	Cluster    Cluster                    `json:"cluster,omitempty"`
	Components map[string]ComponentToggle `json:"components,omitempty"`
}

// VPC is a shared, region-scoped Linode VPC declared in spec.networks. A VPC is a
// container only — Linode VPCs carry no CIDR (subnets do) — so environments attach
// by name and each carves its own cluster.network.subnetCIDR within it.
type VPC struct {
	Region string `json:"region"`
}

// Instance mirrors copier.yml's questions (upstream_org, instance_repo,
// forge_flavor, llz_version). The renderer feeds these to copier as -d data.
type Instance struct {
	UpstreamOrg     string `json:"upstreamOrg"`
	Repo            string `json:"repo"`
	Forge           string `json:"forge"`
	TemplateVersion string `json:"templateVersion"`
}

// Environment is one deployment: its cluster definition plus the component toggles
// that select which components deploy.
type Environment struct {
	Cluster Cluster `json:"cluster"`
	// Components maps a component name (see components.go) to its toggle. A map (not a
	// fixed struct of named bools) keeps adding a component data-only and is the
	// CRD-friendly `additionalProperties` shape; Validate rejects unknown keys.
	Components map[string]ComponentToggle `json:"components,omitempty"`
}

type ComponentToggle struct {
	Enabled bool `json:"enabled"`
}

// Cluster maps to the three per-env tfvars (cluster, cluster-bootstrap,
// object-storage). The comment on each field names its tfvars key.
type Cluster struct {
	ClusterLabel        string         `json:"clusterLabel"`                  // cluster_label
	Region              string         `json:"region"`                        // region
	K8sVersion          string         `json:"k8sVersion"`                    // k8s_version
	Tags                []string       `json:"tags,omitempty"`                // tags
	NodePool            NodePool       `json:"nodePool"`                      // node_type / node_count / autoscaler_enabled
	ControlPlane        ControlPlane   `json:"controlPlane,omitempty"`        // control_plane_*
	APIServerAllowCIDRs AllowCIDRs     `json:"apiServerAllowCIDRs,omitempty"` // github_runner_ipv4/ipv6_cidrs
	Network             ClusterNetwork `json:"network,omitempty"`             // vpc / vpc_subnet_cidr
	HA                  HA             `json:"ha,omitempty"`                  // ha_role / ha_group
	PromotionRank       int            `json:"promotionRank,omitempty"`       // promotion_rank
	Bootstrap           Bootstrap      `json:"bootstrap"`                     // cluster-bootstrap/<env>.tfvars
	ObjectStorage       ObjectStorage  `json:"objectStorage"`                 // object-storage/<env>.tfvars
}

type NodePool struct {
	Type  string `json:"type"`  // node_type
	Count int    `json:"count"` // node_count
	// AutoscalerEnabled is a pointer so an omitted value leaves the tfvars
	// default rather than forcing false; nil == "don't touch".
	AutoscalerEnabled *bool `json:"autoscalerEnabled,omitempty"` // autoscaler_enabled
}

// ControlPlane fields are pointers so an omitted value leaves the tfvars default
// (both default true in the example) instead of zeroing it.
type ControlPlane struct {
	HighAvailability *bool `json:"highAvailability,omitempty"` // control_plane_high_availability
	AuditLogsEnabled *bool `json:"auditLogsEnabled,omitempty"` // control_plane_audit_logs_enabled
}

type AllowCIDRs struct {
	IPv4 []string `json:"ipv4,omitempty"` // github_runner_ipv4_cidrs
	IPv6 []string `json:"ipv6,omitempty"` // github_runner_ipv6_cidrs
}

// DefaultSubnetCIDR mirrors the cluster TF root's vpc_subnet_cidr default
// (terraform-iac-bootstrap/cluster/variables.tf). The validator resolves an unset
// SubnetCIDR to this when checking overlap, so peers that BOTH omit it are still
// caught (the silent collision).
const DefaultSubnetCIDR = "10.0.0.0/13"

// ClusterNetwork is one env's VPC placement: the shared VPC it attaches to (VPC,
// a spec.networks key; empty → its own dedicated VPC named <cluster_label>-vpc),
// and its own subnet within that VPC (SubnetCIDR → vpc_subnet_cidr; a /13 or /14
// per LKE-E). Subnets sharing a VPC must not overlap; HA peers (different regions,
// so always different VPCs) must also stay distinct (Validate enforces both).
type ClusterNetwork struct {
	VPC        string `json:"vpc,omitempty"`        // ref to a spec.networks key; "" → dedicated VPC
	SubnetCIDR string `json:"subnetCIDR,omitempty"` // vpc_subnet_cidr
}

type HA struct {
	Role  string `json:"role,omitempty"`  // ha_role  (standalone|active|standby)
	Group string `json:"group,omitempty"` // ha_group
}

type Bootstrap struct {
	Name             string    `json:"name"`                       // cluster_name
	DomainSuffix     string    `json:"domainSuffix"`               // cluster_domain
	AplChartVersion  string    `json:"aplChartVersion,omitempty"`  // apl_chart_version
	AplValues        AplValues `json:"aplValues,omitempty"`        // apl_values_repo_*
	AppsRepoRevision string    `json:"appsRepoRevision,omitempty"` // apps_repo_revision
}

type AplValues struct {
	RepoURL  string `json:"repoURL,omitempty"`  // apl_values_repo_url
	Revision string `json:"revision,omitempty"` // apl_values_repo_revision
	Username string `json:"username,omitempty"` // apl_values_repo_username
}

type ObjectStorage struct {
	Cluster         string `json:"cluster"`                   // obj_cluster
	KeyRotationDays int    `json:"keyRotationDays,omitempty"` // obj_key_rotation_days
}

// Env returns the named environment and whether it exists.
func (lz *LandingZone) Env(name string) (Environment, bool) {
	e, ok := lz.Spec.Environments[name]
	return e, ok
}

// EnvNames returns the deployment names sorted, so renderers/migrators emit
// stable output and diffs stay localized to the env being changed.
func (lz *LandingZone) EnvNames() []string {
	names := make([]string, 0, len(lz.Spec.Environments))
	for n := range lz.Spec.Environments {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
