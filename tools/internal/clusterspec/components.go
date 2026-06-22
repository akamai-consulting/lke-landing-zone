package clusterspec

// components.go is the single source of truth for a platform component: one name
// the spec toggles (spec.environments.<env>.components.<name>), mapped to what it
// contributes across BOTH delivery backends an LLZ instance uses:
//
//   - apl-core (Helm umbrella): AplCoreApps are the apps.<key>.enabled flags this
//     component flips in apl-values/<env>/values.yaml.
//   - llz-first-party (Argo/kustomize): ManifestResources + ArgoApps are the
//     entries it adds to apl-values/<env>/manifest/kustomization.yaml and
//     manifest/argocd/kustomization.yaml; Patches are kustomize patches it brings.
//
// A component may use either backend or both (e.g. `harbor` enables apl-core's
// harbor app AND adds the llz harbor-registry-s3 ExternalSecret; `observability`
// enables the apl-core monitoring stack AND adds the loki ExternalSecret + alert
// rules). Adding/grouping a component is a data-only edit here — the validator,
// the manifest renderer, and the values renderer all read this registry.
type Component struct {
	Name string
	// Mandatory components cannot be disabled (the cluster does not converge
	// without them) — Validate enforces enabled:true.
	Mandatory bool
	// DependsOn names components that must also be enabled.
	DependsOn []string
	// AplCoreApps are the apl-core values.yaml apps.<key> entries this component
	// enables (the apl-core "umbrella" backend).
	AplCoreApps []string
	// ManifestResources are the entries this component adds to the parent
	// manifest/kustomization.yaml resources: list (the llz Argo/kustomize backend).
	ManifestResources []string
	// ArgoApps are the entries this component adds to manifest/argocd/kustomization.yaml
	// resources: (the wave-ordered component Applications + the AppProject).
	ArgoApps []string
	// Patches are kustomize patches this component contributes to the parent
	// manifest/kustomization.yaml patches: list.
	Patches []Patch
	// DefaultDisabled components default to enabled:false (e.g. dns, applied
	// separately by bootstrap-dns.yml and never in the synced tree).
	DefaultDisabled bool
}

// Patch is one kustomize strategic-merge/JSON patch entry (path + target).
type Patch struct {
	Path                       string
	Group, Version, Kind, Name string
}

// Components is the ordered registry. Order is the rendering order for the
// kustomization resources: lists and the values apps flips.
var Components = []Component{
	{
		Name:              "argocd",
		Mandatory:         true,
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
		// apl-core's monitoring stack + the llz glue (loki S3 ExternalSecret, alert
		// rules) that rides with it.
		Name:        "observability",
		AplCoreApps: []string{"prometheus", "alertmanager", "grafana", "loki", "otel"},
		ManifestResources: []string{
			"observability/loki-object-store-externalsecret.yaml",
			"observability/prometheus-rules/openbao-alerts.yaml",
			"observability/prometheus-rules/support-plane-alerts.yaml",
		},
	},
	{
		Name:              "harbor",
		AplCoreApps:       []string{"harbor"},
		ManifestResources: []string{"harbor/harbor-registry-s3-externalsecret.yaml"},
	},
	{
		// apl-core policy engine (Kyverno + policy-reporter). apl-core-only.
		Name:        "policyEngine",
		AplCoreApps: []string{"kyverno", "policy-reporter"},
	},
	{
		// apl-core image scanning (Trivy). apl-core-only.
		Name:        "imageScanning",
		AplCoreApps: []string{"trivy"},
	},
	{
		// In-cluster Gitea — apl-core-only, currently required by apl-core's global
		// gitops app (see apl-values values.yaml note). Kept enabled by default.
		Name:        "gitea",
		AplCoreApps: []string{"gitea"},
	},
	{
		// Applied separately by .github/workflows/bootstrap-dns.yml once the
		// operator seeds the Linode DNS token; never part of the Argo-synced tree.
		Name:            "dns",
		DefaultDisabled: true,
	},
}

// componentByName indexes Components for lookup.
var componentByName = func() map[string]Component {
	m := make(map[string]Component, len(Components))
	for _, c := range Components {
		m[c.Name] = c
	}
	return m
}()

// KnownComponent reports whether name is a registered component.
func KnownComponent(name string) bool {
	_, ok := componentByName[name]
	return ok
}

// LookupComponent returns the registry entry for name.
func LookupComponent(name string) (Component, bool) {
	c, ok := componentByName[name]
	return c, ok
}

// ComponentEnabled reports whether the toggles enable name. A name absent from
// the map is treated as its built-in default (Defaults() fills the map, so this
// is mainly for renderers that receive a partial map).
func ComponentEnabled(toggles map[string]ComponentToggle, name string) bool {
	if t, ok := toggles[name]; ok {
		return t.Enabled
	}
	c, ok := componentByName[name]
	return ok && !c.DefaultDisabled
}
