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
		// Components default to all-enabled, except the DefaultDisabled ones (dns).
		// A nil/empty map gets the full default set; a partial map only fills in
		// components the author didn't mention (so an explicit enabled:false sticks).
		if env.Components == nil {
			env.Components = map[string]ComponentToggle{}
		}
		for _, r := range Components {
			t, set := env.Components[r.Name]
			if !set {
				env.Components[r.Name] = ComponentToggle{Enabled: boolPtr(!r.DefaultDisabled)}
				continue
			}
			// A toggle that sets only sizing (Enabled nil) resolves to the built-in
			// default so the rest of the pipeline sees a complete, non-nil state.
			if t.Enabled == nil {
				t.Enabled = boolPtr(!r.DefaultDisabled)
				env.Components[r.Name] = t
			}
		}
		lz.Spec.Environments[name] = env
	}
}
