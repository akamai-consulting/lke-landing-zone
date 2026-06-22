package clusterspec

// merge.go implements the split layout's inheritance: shared `spec.defaults`
// merged UNDER each environment, so a per-env value wins and an unset field
// falls back to the default. "Provides a value" uses the same rule the tfvars
// mapping emits on — non-empty string, non-nil pointer/slice, positive int — so
// an omitted field inherits while an explicit zero (e.g. an empty CIDR list, or
// autoscaler:false) is honored. The merge is pure and idempotent: re-merging an
// already-merged environment with the same defaults is a no-op, because the
// override values are already populated and continue to win.

// applyInheritance folds spec.defaults into every environment in place. Called
// after the environments/*.yaml files are assembled into spec.environments and before
// the built-in Defaults(), so the precedence is env > defaults > built-in.
func (lz *LandingZone) applyInheritance() {
	d := lz.Spec.Defaults
	for name, env := range lz.Spec.Environments {
		lz.Spec.Environments[name] = mergeEnvironment(d, env)
	}
}

// mergeEnvironment returns env with the shared defaults filled in underneath it.
func mergeEnvironment(d Defaults, env Environment) Environment {
	env.Cluster = mergeCluster(d.Cluster, env.Cluster)
	env.Recipes = mergeRecipes(d.Recipes, env.Recipes)
	return env
}

// mergeCluster returns over with base filling any field over leaves unset.
func mergeCluster(base, over Cluster) Cluster {
	out := over
	out.ClusterLabel = pickStr(base.ClusterLabel, over.ClusterLabel)
	out.Region = pickStr(base.Region, over.Region)
	out.K8sVersion = pickStr(base.K8sVersion, over.K8sVersion)
	out.Tags = pickSlice(base.Tags, over.Tags)
	out.PromotionRank = pickInt(base.PromotionRank, over.PromotionRank)

	out.NodePool.Type = pickStr(base.NodePool.Type, over.NodePool.Type)
	out.NodePool.Count = pickInt(base.NodePool.Count, over.NodePool.Count)
	out.NodePool.AutoscalerEnabled = pickBoolPtr(base.NodePool.AutoscalerEnabled, over.NodePool.AutoscalerEnabled)

	out.ControlPlane.HighAvailability = pickBoolPtr(base.ControlPlane.HighAvailability, over.ControlPlane.HighAvailability)
	out.ControlPlane.AuditLogsEnabled = pickBoolPtr(base.ControlPlane.AuditLogsEnabled, over.ControlPlane.AuditLogsEnabled)

	out.APIServerAllowCIDRs.IPv4 = pickSlice(base.APIServerAllowCIDRs.IPv4, over.APIServerAllowCIDRs.IPv4)
	out.APIServerAllowCIDRs.IPv6 = pickSlice(base.APIServerAllowCIDRs.IPv6, over.APIServerAllowCIDRs.IPv6)

	out.Network.VPC = pickStr(base.Network.VPC, over.Network.VPC)
	out.Network.SubnetCIDR = pickStr(base.Network.SubnetCIDR, over.Network.SubnetCIDR)

	out.HA.Role = pickStr(base.HA.Role, over.HA.Role)
	out.HA.Group = pickStr(base.HA.Group, over.HA.Group)

	out.Bootstrap.Name = pickStr(base.Bootstrap.Name, over.Bootstrap.Name)
	out.Bootstrap.DomainSuffix = pickStr(base.Bootstrap.DomainSuffix, over.Bootstrap.DomainSuffix)
	out.Bootstrap.AplChartVersion = pickStr(base.Bootstrap.AplChartVersion, over.Bootstrap.AplChartVersion)
	out.Bootstrap.AppsRepoRevision = pickStr(base.Bootstrap.AppsRepoRevision, over.Bootstrap.AppsRepoRevision)
	out.Bootstrap.AplValues.RepoURL = pickStr(base.Bootstrap.AplValues.RepoURL, over.Bootstrap.AplValues.RepoURL)
	out.Bootstrap.AplValues.Revision = pickStr(base.Bootstrap.AplValues.Revision, over.Bootstrap.AplValues.Revision)
	out.Bootstrap.AplValues.Username = pickStr(base.Bootstrap.AplValues.Username, over.Bootstrap.AplValues.Username)

	out.ObjectStorage.Cluster = pickStr(base.ObjectStorage.Cluster, over.ObjectStorage.Cluster)
	out.ObjectStorage.KeyRotationDays = pickInt(base.ObjectStorage.KeyRotationDays, over.ObjectStorage.KeyRotationDays)
	return out
}

// mergeRecipes overlays per-env toggles on the shared defaults (env wins
// per-key). Returns nil when neither side sets anything, so the built-in
// Defaults() still applies the full default recipe set.
func mergeRecipes(base, over map[string]RecipeToggle) map[string]RecipeToggle {
	if base == nil && over == nil {
		return nil
	}
	out := make(map[string]RecipeToggle, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func pickStr(base, over string) string {
	if over != "" {
		return over
	}
	return base
}

func pickInt(base, over int) int {
	if over > 0 {
		return over
	}
	return base
}

func pickBoolPtr(base, over *bool) *bool {
	if over != nil {
		return over
	}
	return base
}

func pickSlice(base, over []string) []string {
	if over != nil {
		return over
	}
	return base
}
