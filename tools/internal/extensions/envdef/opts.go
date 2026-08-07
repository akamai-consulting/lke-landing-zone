// Package envdef writes an environment's definition into a scaffolded instance —
// the environments/<env>.yaml the spec is assembled from, and the landingzone.yaml
// that has to exist before it.
//
// Opts CAME WITH IT, and that is the whole reason this move was possible. The
// struct was `Opts` in scaffold.go, read by five files, and it is the
// boundary between `llz env add`'s FLAG SET — which is package main's, and stays
// there — and the WRITER that turns those answers into YAML. Same call already
// made for internal/bootstrapcluster's BootstrapFlags: eighteen fields that are
// each a genuine command-line input is a PARAMETER, not a configuration surface.
package envdef

// Opts mirrors new-deployment.sh's flags, plus the ADOPTER-MUST-SET values
// that used to be "open the file and edit" steps (item 8): supplying them here
// makes `env add → tokens → build` a guided path instead of a hand-edit detour.
type Opts struct {
	TemplateEnv   string
	Region        string
	RegionShort   string
	ClusterDomain string
	ObjCluster    string
	// must-set values (empty = leave the scaffold placeholder for the operator)
	K8sVersion       string
	NodeType         string // Linode node type for the pool
	NodeCount        string // pool size (integer; string so empty = leave default)
	RunnerIPv4CIDRs  string // comma-separated
	RunnerIPv6CIDRs  string // comma-separated
	AplChartVersion  string
	AplValuesRepoURL string
	HARole           string // active | standby | standalone (default: leave example's standalone)
	HAGroup          string // HA pair id (required for active/standby)
	Network          string // shared VPC name (spec.networks) to attach to; "" = dedicated VPC
	SubnetCIDR       string // cluster.network.subnetCIDR (/13 or /14); "" = default
	PromotionRank    int    // code-promotion pipeline position; 0 = leave example's 0 (not in a pipeline)
	DryRun           bool
}
