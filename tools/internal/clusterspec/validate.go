package clusterspec

import (
	"fmt"
	"sort"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/validate"
)

// Validate returns every problem with the spec (not just the first), so an
// operator fixing llz.yaml sees the whole list. It reuses the same pure
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
	return errs
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

	errs = append(errs, validateRecipes(name, env.Recipes)...)
	return errs
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
