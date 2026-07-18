package clusterspec

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/validate"
)

// durationRe matches a Prometheus/Go-ish retention duration (e.g. 7d, 720h, 30m);
// quantityRe matches a Kubernetes storage quantity (e.g. 10Gi, 500Mi, 1Ti, 20G).
var (
	durationRe = regexp.MustCompile(`^[0-9]+(ns|us|µs|ms|s|m|h|d|w|y)$`)
	quantityRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E|m)?$`)
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
	errs = append(errs, validateAlerting(lz.Spec.Alerting)...)
	return errs
}

// validateAlerting checks spec.alerting: receivers may only name channels the
// landing zone wires ("slack", "none" — msteams et al. put secret webhook URLs
// in committed values, which the OpenBao-backed slack path deliberately
// avoids), "none" cannot be combined with a real channel, and slack channel
// overrides only make sense when the slack receiver is selected.
func validateAlerting(a Alerting) []error {
	var errs []error
	hasSlack, hasNone := false, false
	for _, r := range a.Receivers {
		switch r {
		case "slack":
			hasSlack = true
		case "none":
			hasNone = true
		default:
			errs = append(errs, fmt.Errorf("alerting.receivers: %q is not supported (use slack or none; msteams would put its secret webhook URLs in committed values)", r))
		}
	}
	if hasNone && hasSlack {
		errs = append(errs, fmt.Errorf("alerting.receivers: none cannot be combined with slack"))
	}
	if (a.Slack.Channel != "" || a.Slack.ChannelCrit != "") && !hasSlack {
		errs = append(errs, fmt.Errorf("alerting.slack.* is set but receivers does not include slack"))
	}
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

	// The apl-core-owned branch (apl-operator commits its env/ tree here) and the
	// LLZ-owned apps branch (platform-bootstrap reads here) MUST differ. If they
	// coincide, apl-operator's additive env/ commits land on the same branch
	// platform-bootstrap syncs, and it reads an operator-authored commit — the exact
	// pre-ADR production wedge (v1beta1 ExternalSecrets that 6.x's ESO rejects,
	// convergence hard-fails). The defaults (apl-<env> vs main) never collide; this
	// catches an override that reintroduces the collision — e.g. aplValues.revision:
	// main, or appsRepoRevision pointed at apl-<env>. Compare EFFECTIVE values so a
	// defaulted-and-an-explicit form that resolve equal are still caught. See
	// docs/designs/apl-core-values-branch-isolation.md.
	if aplBranch, appsRev := c.Bootstrap.AplValuesBranch(name), c.Bootstrap.AppsRevision(); aplBranch == appsRev {
		errs = append(errs, prefix(
			"cluster.bootstrap: aplValues.revision (%q) and appsRepoRevision (%q) resolve to the same branch %q — "+
				"apl-operator commits its env/ tree to the former while platform-bootstrap reads the latter, so sharing one "+
				"branch reproduces the pre-ADR converge wedge. Leave aplValues.revision unset (defaults to the apl-core-owned "+
				"apl-%s) or point it at a branch other than %q. See docs/designs/apl-core-values-branch-isolation.md",
			c.Bootstrap.AplValues.Revision, c.Bootstrap.AppsRepoRevision, aplBranch, name, appsRev))
	}

	errs = append(errs, validateComponents(name, env.Components)...)
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

func validateComponents(env string, components map[string]ComponentToggle) []error {
	var errs []error
	// Stable iteration for deterministic error ordering.
	names := make([]string, 0, len(components))
	for n := range components {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		if !KnownComponent(n) {
			errs = append(errs, fmt.Errorf("environments.%s.components.%s: unknown component (known: %s)", env, n, knownComponentList()))
			continue
		}
		errs = append(errs, validateComponentSizing(env, n, components[n])...)
	}
	for _, r := range Components {
		enabled := ComponentEnabled(components, r.Name)
		if r.Mandatory && !enabled {
			errs = append(errs, fmt.Errorf("environments.%s.components.%s is mandatory and cannot be disabled", env, r.Name))
		}
		if enabled {
			for _, dep := range r.DependsOn {
				if !ComponentEnabled(components, dep) {
					errs = append(errs, fmt.Errorf("environments.%s.components.%s requires component %q to be enabled", env, r.Name, dep))
				}
			}
			// The broad-PAT rotator's CronJob errors at runtime without its account-
			// wide label + deployment list; require them at render time (the env patch
			// would otherwise fill BROAD_PAT_* with empty strings). See RenderBroadPATEnvPatch.
			if r.Name == "broadPatRotator" {
				t := components[r.Name]
				if t.BroadPATLabel == "" || t.BroadPATDeployments == "" {
					errs = append(errs, fmt.Errorf("environments.%s.components.broadPatRotator requires broadPATLabel and broadPATDeployments when enabled (the account-wide Linode PAT label + the space-separated infra-<d> environments to publish into)", env))
				}
			}
		}
	}
	return errs
}

// sizingKnobs maps each component to the spec.components sizing fields it reads;
// a knob set on any other component is a likely mistake (it would be silently
// ignored). Components absent from the map accept no sizing.
var sizingKnobs = map[string][]string{
	"observability":   {"retention", "storage", "replicas"},
	"harbor":          {"registryStorage"},
	"broadPatRotator": {"broadPATLabel", "broadPATDeployments"},
}

// validateComponentSizing rejects sizing knobs set on a component that does not
// read them, and bad capacity/duration formats.
func validateComponentSizing(env, name string, t ComponentToggle) []error {
	var errs []error
	allowed := map[string]bool{}
	for _, k := range sizingKnobs[name] {
		allowed[k] = true
	}
	set := map[string]string{} // knob → value, for format checks below
	if t.Retention != "" {
		set["retention"] = t.Retention
	}
	if t.Storage != "" {
		set["storage"] = t.Storage
	}
	if t.RegistryStorage != "" {
		set["registryStorage"] = t.RegistryStorage
	}
	if t.BroadPATLabel != "" {
		set["broadPATLabel"] = t.BroadPATLabel
	}
	if t.BroadPATDeployments != "" {
		set["broadPATDeployments"] = t.BroadPATDeployments
	}
	if t.Replicas != nil {
		set["replicas"] = ""
	}
	for knob := range set {
		if !allowed[knob] {
			errs = append(errs, fmt.Errorf("environments.%s.components.%s: %s is not a valid setting for this component (it reads: %s)", env, name, knob, knobList(name)))
		}
	}
	if t.Replicas != nil && *t.Replicas < 1 {
		errs = append(errs, fmt.Errorf("environments.%s.components.%s.replicas must be >= 1", env, name))
	}
	if t.Retention != "" && !durationRe.MatchString(t.Retention) {
		errs = append(errs, fmt.Errorf("environments.%s.components.%s.retention %q is not a duration (e.g. 7d, 720h)", env, name, t.Retention))
	}
	for knob, v := range map[string]string{"storage": t.Storage, "registryStorage": t.RegistryStorage} {
		if v != "" && !quantityRe.MatchString(v) {
			errs = append(errs, fmt.Errorf("environments.%s.components.%s.%s %q is not a storage quantity (e.g. 10Gi)", env, name, knob, v))
		}
	}
	return errs
}

func knobList(name string) string {
	if ks := sizingKnobs[name]; len(ks) > 0 {
		return strings.Join(ks, ", ")
	}
	return "(none)"
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

func knownComponentList() string {
	names := make([]string, len(Components))
	for i, r := range Components {
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
