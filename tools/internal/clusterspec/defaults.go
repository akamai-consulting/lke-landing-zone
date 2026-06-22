package clusterspec

// Defaults fills the derived/implied fields so the renderer and validator see a
// complete spec. It is idempotent. Defaults are deliberately minimal — fields
// the author omits and that have a sensible tfvars-example default (the two
// control-plane bools, autoscaler) are left nil so the renderer leaves the
// example value untouched rather than forcing a zero.
func (lz *LandingZone) Defaults() {
	for name, env := range lz.Spec.Environments {
		// domainSuffix defaults to "<env>.internal" (mirrors scaffold.go's
		// clusterDomain default in runEnvAdd).
		if env.Cluster.Bootstrap.DomainSuffix == "" {
			env.Cluster.Bootstrap.DomainSuffix = name + ".internal"
		}
		// Recipes default to all-enabled, except the DefaultDisabled ones (dns).
		// A nil/empty map gets the full default set; a partial map only fills in
		// recipes the author didn't mention (so an explicit enabled:false sticks).
		if env.Recipes == nil {
			env.Recipes = map[string]RecipeToggle{}
		}
		for _, r := range Recipes {
			if _, set := env.Recipes[r.Name]; !set {
				env.Recipes[r.Name] = RecipeToggle{Enabled: !r.DefaultDisabled}
			}
		}
		lz.Spec.Environments[name] = env
	}
}
