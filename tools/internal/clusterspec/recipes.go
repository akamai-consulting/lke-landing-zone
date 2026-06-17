package clusterspec

// recipes.go is the single source of truth mapping a recipe name to what it
// contributes to a rendered apl-values/<env>/manifest tree. Both the validator
// (known-key set + dependency rules, validate.go) and the recipe renderer
// (PR2, render.go) read this registry, so adding/removing a component is a
// data-only edit here. The mapping is derived from the example overlay:
//   - ManifestResources → apl-values/example/manifest/kustomization.yaml resources:
//   - ArgoApps          → apl-values/example/manifest/argocd/kustomization.yaml resources:
type Recipe struct {
	Name string
	// Mandatory recipes cannot be disabled (the cluster does not converge
	// without them) — Validate enforces enabled:true.
	Mandatory bool
	// DependsOn names recipes that must also be enabled.
	DependsOn []string
	// ManifestResources are the entries this recipe adds to the parent
	// kustomization.yaml resources: list.
	ManifestResources []string
	// ArgoApps are the entries this recipe adds to argocd/kustomization.yaml
	// resources: (the wave-ordered component Applications + the AppProject).
	ArgoApps []string
	// Patches are the strategic-merge/target patches this recipe adds to the
	// parent kustomization.yaml patches: list (e.g. the per-env REGION_SHORT
	// override on the volume-labeler CronJob).
	Patches []Patch
	// DefaultDisabled recipes default to enabled:false (e.g. dns, which is
	// applied separately by bootstrap-dns.yml and never lives in the synced tree).
	DefaultDisabled bool
}

// Patch is one kustomize patch entry (path + target selector), mirroring the
// `patches:` block in apl-values/example/manifest/kustomization.yaml.
type Patch struct {
	Path    string
	Group   string
	Version string
	Kind    string
	Name    string
}

// Recipes is the ordered registry. Order is the rendering order for the
// kustomization resources: lists.
var Recipes = []Recipe{
	{
		Name:      "argocd",
		Mandatory: true,
		// The argocd/ dir (AppProject + applications) in the parent tree.
		ManifestResources: []string{"argocd"},
		ArgoApps:          []string{"platform-support-project.yaml"},
	},
	{
		Name:      "clusterFoundation",
		Mandatory: true, // wave -20: namespaces, default-deny NPs, CoreDNS, storage
		ArgoApps:  []string{"applications/cluster-foundation.yaml"},
	},
	{
		Name: "externalSecrets",
		ManifestResources: []string{
			"external-secrets/network-policies.yaml",
			"external-secrets/cluster-secret-store-openbao.yaml",
		},
		ArgoApps: []string{"applications/external-secrets-operator.yaml"},
	},
	{
		Name:              "argoWorkflows",
		ManifestResources: []string{"argo-workflows/network-policies.yaml"},
		ArgoApps:          []string{"applications/argo-workflows.yaml"},
	},
	{
		Name:              "argoEvents",
		ManifestResources: []string{"argo-events/network-policies.yaml"},
		ArgoApps:          []string{"applications/argo-events.yaml"},
	},
	{
		Name:              "certManager",
		ManifestResources: []string{"cert-manager/openbao-bootstrap-ca.yaml"},
		ArgoApps:          []string{"applications/cert-automation.yaml"},
	},
	{
		Name:              "openbao",
		DependsOn:         []string{"externalSecrets", "certManager"},
		ManifestResources: []string{"openbao/openbao-cert-watcher.yaml"},
		ArgoApps: []string{
			"applications/eso-cert-watcher.yaml",
			"applications/openbao.yaml",
		},
	},
	{
		Name:              "volumeLabeler",
		ManifestResources: []string{"linode-volume-labeler"},
		Patches: []Patch{{
			Path:    "linode-volume-labeler-region-patch.yaml",
			Group:   "batch",
			Version: "v1",
			Kind:    "CronJob",
			Name:    "linode-volume-labeler",
		}},
	},
	{
		Name: "observability",
		ManifestResources: []string{
			"observability/loki-object-store-externalsecret.yaml",
			"observability/prometheus-rules/openbao-alerts.yaml",
			"observability/prometheus-rules/support-plane-alerts.yaml",
		},
	},
	{
		Name:              "harbor",
		ManifestResources: []string{"harbor/harbor-registry-s3-externalsecret.yaml"},
	},
	{
		// Applied separately by .github/workflows/bootstrap-dns.yml once the
		// operator seeds the Linode DNS token; never part of the Argo-synced tree.
		Name:            "dns",
		DefaultDisabled: true,
	},
}

// recipeByName indexes Recipes for lookup.
var recipeByName = func() map[string]Recipe {
	m := make(map[string]Recipe, len(Recipes))
	for _, r := range Recipes {
		m[r.Name] = r
	}
	return m
}()

// KnownRecipe reports whether name is a registered recipe.
func KnownRecipe(name string) bool {
	_, ok := recipeByName[name]
	return ok
}

// LookupRecipe returns the registry entry for name.
func LookupRecipe(name string) (Recipe, bool) {
	r, ok := recipeByName[name]
	return r, ok
}

// RecipeEnabled reports whether the named recipe is toggled on in the (defaulted)
// recipe map.
func RecipeEnabled(recipes map[string]RecipeToggle, name string) bool {
	t, ok := recipes[name]
	return ok && t.Enabled
}
