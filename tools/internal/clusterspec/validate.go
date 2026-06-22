package clusterspec

import (
	"fmt"
	"net"
	"sort"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/validate"
)

// Validate returns every problem with the spec (not just the first), so an
// operator fixing the spec sees the whole list. It reuses the same pure
// validators the CLI enforces (internal/validate) so the spec and `llz env add`
// share one contract. Call Defaults first (Load/Decode do).
func (lz *LandingZone) Validate() []error {
	var errs []error

	if lz.APIVersion != APIVersion {
		errs = append(errs, fmt.Errorf("apiVersion %q invalid (want %q)", lz.APIVersion, APIVersion))
	}
	if lz.Kind != Kind {
		errs = append(errs, fmt.Errorf("kind %q invalid (want %q)", lz.Kind, Kind))
	}
	if lz.Metadata.Name == "" {
		errs = append(errs, fmt.Errorf("metadata.name is required (the instance name)"))
	}

	errs = append(errs, validateInstance(lz.Spec.Instance)...)

	if len(lz.Spec.Environments) == 0 {
		errs = append(errs, fmt.Errorf("spec.environments is empty — declare at least one deployment"))
	}
	for _, name := range lz.EnvNames() {
		errs = append(errs, validateEnv(name, lz.Spec.Environments[name])...)
	}
	errs = append(errs, validateHAGroups(lz)...)
	errs = append(errs, validateNetworks(lz)...)
	errs = append(errs, validateHAVPCCIDRs(lz)...)
	return errs
}

// validateNetworks checks the shared-VPC model: every spec.networks entry needs a
// region; an env's cluster.network.vpc must reference a declared network in the
// SAME region (Linode VPCs cannot span regions); and environments sharing one VPC
// must have non-overlapping subnet CIDRs (Linode rejects overlapping subnets in a
// VPC). An unset subnet resolves to DefaultSubnetCIDR, so two envs both omitting it
// on a shared VPC are still caught. Dedicated-VPC envs (no .vpc) are unconstrained.
func validateNetworks(lz *LandingZone) []error {
	var errs []error
	for _, name := range sortedKeys(lz.Spec.Networks) {
		if err := validate.EnvName(name); err != nil {
			errs = append(errs, fmt.Errorf("networks key: %w", err))
		}
		if lz.Spec.Networks[name].Region == "" {
			errs = append(errs, fmt.Errorf("networks.%s.region is required", name))
		}
	}

	type member struct{ name, cidr string }
	shared := map[string][]member{}
	for _, name := range lz.EnvNames() {
		c := lz.Spec.Environments[name].Cluster
		ref := c.Network.VPC
		if ref == "" {
			continue // dedicated VPC — isolated, no cross-env constraint
		}
		vpc, ok := lz.Spec.Networks[ref]
		if !ok {
			errs = append(errs, fmt.Errorf("environments.%s.cluster.network.vpc %q is not declared in spec.networks", name, ref))
			continue
		}
		if vpc.Region != "" && c.Region != "" && vpc.Region != c.Region {
			errs = append(errs, fmt.Errorf("environments.%s is in region %q but attaches to network %q (region %q) — Linode VPCs cannot span regions", name, c.Region, ref, vpc.Region))
			continue
		}
		cidr := c.Network.SubnetCIDR
		if cidr == "" {
			cidr = DefaultSubnetCIDR
		}
		shared[ref] = append(shared[ref], member{name, cidr})
	}
	for _, ref := range sortedKeys(shared) {
		ms := shared[ref]
		for i := 0; i < len(ms); i++ {
			for j := i + 1; j < len(ms); j++ {
				if cidrsOverlap(ms[i].cidr, ms[j].cidr) {
					errs = append(errs, fmt.Errorf(
						"network %q: %s (%s) and %s (%s) have overlapping subnet CIDRs — "+
							"subnets in one VPC must not overlap; give each a distinct cluster.network.subnetCIDR",
						ref, ms[i].name, ms[i].cidr, ms[j].name, ms[j].cidr))
				}
			}
		}
	}
	return errs
}

// sortedKeys returns a map's keys in sorted order, for deterministic error output.
func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func validateInstance(in Instance) []error {
	var errs []error
	if in.UpstreamOrg == "" {
		errs = append(errs, fmt.Errorf("spec.instance.upstreamOrg is required"))
	}
	if in.Repo == "" {
		errs = append(errs, fmt.Errorf("spec.instance.repo is required (<owner>/<name>)"))
	}
	if in.TemplateVersion == "" {
		errs = append(errs, fmt.Errorf("spec.instance.templateVersion is required (the pinned llz release, or 'main')"))
	}
	if err := validate.Forge(in.Forge); err != nil {
		errs = append(errs, fmt.Errorf("spec.instance.%w", err))
	}
	return errs
}

func validateEnv(name string, env Environment) []error {
	var errs []error
	prefix := func(format string, a ...any) error {
		return fmt.Errorf("environments.%s.%s", name, fmt.Sprintf(format, a...))
	}
	if err := validate.EnvName(name); err != nil {
		errs = append(errs, fmt.Errorf("environments key: %w", err))
	}

	c := env.Cluster
	if c.ClusterLabel == "" {
		errs = append(errs, prefix("cluster.clusterLabel is required"))
	}
	if c.Region == "" {
		errs = append(errs, prefix("cluster.region is required"))
	}
	if c.K8sVersion == "" {
		errs = append(errs, prefix("cluster.k8sVersion is required"))
	}
	if c.NodePool.Type == "" {
		errs = append(errs, prefix("cluster.nodePool.type is required"))
	}
	if c.NodePool.Count <= 0 {
		errs = append(errs, prefix("cluster.nodePool.count must be > 0"))
	}
	if c.Bootstrap.Name == "" {
		errs = append(errs, prefix("cluster.bootstrap.name is required"))
	}
	if err := validate.HATopology(c.HA.Role, c.HA.Group, "cluster.ha.role", "cluster.ha.group"); err != nil {
		errs = append(errs, prefix("%v", err))
	}
	if err := validate.OBJClusterID(c.ObjectStorage.Cluster); err != nil {
		errs = append(errs, prefix("cluster.objectStorage.cluster: %v", err))
	}
	if c.Network.SubnetCIDR != "" {
		if err := validateSubnetCIDR(c.Network.SubnetCIDR); err != nil {
			errs = append(errs, prefix("cluster.network.subnetCIDR: %v", err))
		}
	}

	errs = append(errs, validateRecipes(name, env.Recipes)...)
	return errs
}

// validateSubnetCIDR enforces the LKE-E worker-subnet contract: a valid IPv4
// CIDR with a /13 or /14 prefix (see the cluster TF root's vpc_subnet_cidr).
func validateSubnetCIDR(cidr string) error {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("%q is not a valid CIDR", cidr)
	}
	if ip.To4() == nil {
		return fmt.Errorf("%q must be an IPv4 CIDR", cidr)
	}
	if ones, _ := ipnet.Mask.Size(); ones != 13 && ones != 14 {
		return fmt.Errorf("%q must be a /13 or /14 (LKE-E requirement)", cidr)
	}
	return nil
}

// validateHAVPCCIDRs enforces that the members of an OpenBao HA group use
// NON-overlapping VPC subnet CIDRs. Linode VPCs are region-scoped and never
// shared, so two peers can route to each other (peering / a transit mesh) only if
// their ranges are distinct. An unset value resolves to DefaultSubnetCIDR, so
// two peers that BOTH omit it are still caught (the silent dual-region collision).
// Non-HA envs are unconstrained — their VPCs are isolated and never coexist.
func validateHAVPCCIDRs(lz *LandingZone) []error {
	type member struct{ name, cidr string }
	groups := map[string][]member{}
	for _, name := range lz.EnvNames() {
		c := lz.Spec.Environments[name].Cluster
		if c.HA.Group == "" {
			continue
		}
		cidr := c.Network.SubnetCIDR
		if cidr == "" {
			cidr = DefaultSubnetCIDR
		}
		groups[c.HA.Group] = append(groups[c.HA.Group], member{name, cidr})
	}

	var errs []error
	gnames := make([]string, 0, len(groups))
	for g := range groups {
		gnames = append(gnames, g)
	}
	sort.Strings(gnames)
	for _, g := range gnames {
		ms := groups[g]
		for i := 0; i < len(ms); i++ {
			for j := i + 1; j < len(ms); j++ {
				if cidrsOverlap(ms[i].cidr, ms[j].cidr) {
					errs = append(errs, fmt.Errorf(
						"ha group %q: %s (%s) and %s (%s) have overlapping VPC subnet CIDRs — "+
							"Linode VPCs are region-scoped, so give each a distinct cluster.network.subnetCIDR",
						g, ms[i].name, ms[i].cidr, ms[j].name, ms[j].cidr))
				}
			}
		}
	}
	return errs
}

// cidrsOverlap reports whether two CIDR blocks intersect. CIDR blocks are either
// disjoint or nested, so it suffices to check whether either contains the other's
// network address. Unparseable inputs are treated as non-overlapping (the format
// error is reported per-env by validateSubnetCIDR).
func cidrsOverlap(a, b string) bool {
	_, na, errA := net.ParseCIDR(a)
	_, nb, errB := net.ParseCIDR(b)
	if errA != nil || errB != nil {
		return false
	}
	return na.Contains(nb.IP) || nb.Contains(na.IP)
}

func validateRecipes(env string, recipes map[string]RecipeToggle) []error {
	var errs []error
	// Stable iteration for deterministic error ordering.
	names := make([]string, 0, len(recipes))
	for n := range recipes {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		if !KnownRecipe(n) {
			errs = append(errs, fmt.Errorf("environments.%s.recipes.%s: unknown recipe (known: %s)", env, n, knownRecipeList()))
		}
	}
	for _, r := range Recipes {
		t, set := recipes[r.Name]
		enabled := set && t.Enabled
		if r.Mandatory && !enabled {
			errs = append(errs, fmt.Errorf("environments.%s.recipes.%s is mandatory and cannot be disabled", env, r.Name))
		}
		if enabled {
			for _, dep := range r.DependsOn {
				if dt, ok := recipes[dep]; !ok || !dt.Enabled {
					errs = append(errs, fmt.Errorf("environments.%s.recipes.%s requires recipe %q to be enabled", env, r.Name, dep))
				}
			}
		}
	}
	return errs
}

// validateHAGroups enforces the cross-environment pairing: every non-empty
// ha.group must have exactly one active and one standby (mirrors validateTopology
// in cmd/llz/topology.go, but over the spec's environments).
func validateHAGroups(lz *LandingZone) []error {
	type pair struct{ actives, standbys []string }
	groups := map[string]*pair{}
	for _, name := range lz.EnvNames() {
		ha := lz.Spec.Environments[name].Cluster.HA
		if ha.Group == "" {
			continue
		}
		p := groups[ha.Group]
		if p == nil {
			p = &pair{}
			groups[ha.Group] = p
		}
		switch ha.Role {
		case validate.RoleActive:
			p.actives = append(p.actives, name)
		case validate.RoleStandby:
			p.standbys = append(p.standbys, name)
		}
	}
	var errs []error
	gnames := make([]string, 0, len(groups))
	for g := range groups {
		gnames = append(gnames, g)
	}
	sort.Strings(gnames)
	for _, g := range gnames {
		p := groups[g]
		if len(p.actives) != 1 || len(p.standbys) != 1 {
			errs = append(errs, fmt.Errorf("ha group %q must have exactly one active and one standby; got active=%v standby=%v", g, p.actives, p.standbys))
		}
	}
	return errs
}

func knownRecipeList() string {
	names := make([]string, len(Recipes))
	for i, r := range Recipes {
		names[i] = r.Name
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
