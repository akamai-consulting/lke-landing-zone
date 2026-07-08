package clusterspec

// components.go is the single source of truth for a platform component: one name
// the spec toggles (spec.environments.<env>.components.<name>), mapped to what it
// contributes across BOTH delivery backends an LLZ instance uses:
//
//   - apl-core (Helm umbrella): AplCoreApps are the apps.<key>.enabled flags this
//     component flips in apl-values/<env>/values.yaml.
//   - llz-first-party (Argo/kustomize): ManifestResources + ArgoApps are the
//     resources its shared kustomize Component (apl-values/components/<name>/, or the
//     apl-values/_shared/manifest base for mandatory components) lists — the thin
//     per-env manifest/kustomization.yaml pulls them in via components:. Patches are
//     kustomize patches it brings.
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
	// ArgoApps are the wave-ordered component Applications (+ the AppProject) this
	// component's shared kustomize Component lists — the per-env overlay pulls them
	// in via components: (mandatory components list them in the _shared base instead).
	ArgoApps []string
	// Patches are kustomize patches this component contributes. For a plain
	// (in-bundle) component they land in the parent manifest/kustomization.yaml
	// patches: list; for a CarvedApp component they land in that App's own per-env
	// apps/<name>/kustomization.yaml instead (the patched resource moves with it).
	Patches []Patch
	// CarvedApp, when non-nil, makes `llz render` emit a standalone git-path Argo CD
	// Application (health-inert in the platform-bootstrap tree) that health-gates
	// this component's OWN content — instead of merging its resources as a raw
	// components: entry that shares platform-bootstrap's sync/health fate. A Degraded
	// resource then fails only its own App (the #142/#163 blast-radius class). See
	// docs/designs/blast-radius-decomposition.md.
	CarvedApp *CarvedApp
	// DefaultDisabled components default to enabled:false.
	DefaultDisabled bool
}

// Patch is one kustomize strategic-merge/JSON patch entry (path + target).
type Patch struct {
	Path                       string
	Group, Version, Kind, Name string
}

// CarvedApp describes the standalone Argo CD Application a decomposed component
// renders into. It is the single source of truth both `llz render` (which emits
// the App CR + its per-env source root) and the wave-dependency-guard (which
// treats AppWave as the cross-Application ordering floor) read.
type CarvedApp struct {
	// AppName is the Application metadata.name (llz-<x>) — also the App CR filename
	// (<AppName>.yaml) rendered into the per-env manifest tree.
	AppName string
	// AppWave is the App-level argocd.argoproj.io/sync-wave on the Application CR.
	// It is the FLOOR for every resource the App carries (Argo cannot sync a carved
	// App's content before the app-of-apps creates the App at this wave) and the key
	// the wave-dependency-guard uses to order one carved App's resources against
	// another's. externalSecrets — the dependency root every consumer's Secret
	// resolution waits on — gets the lowest wave so its content goes up first.
	AppWave int
	// Namespace is the Application spec.destination.namespace fallback for
	// unnamespaced resources; namespaced resources carry their own metadata.namespace
	// (the bundles span several), so this is only a default, not a scope.
	Namespace string
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
		// The `openbao` ClusterSecretStore moved to its OWN Argo CD Application
		// (apl-values/_shared/manifest-secret-store/, applied by the llz-secret-store
		// app in cluster-bootstrap/main.tf) for blast-radius isolation — its
		// first-boot not-ready health used to fail the whole platform-bootstrap sync.
		ManifestResources: []string{
			"external-secrets/network-policies.yaml",
		},
		ArgoApps: []string{"applications/external-secrets-operator.yaml"},
		// The dependency ROOT: its default-deny + ESO-egress NetworkPolicies gate
		// whether ANY ExternalSecret can reach OpenBao, so its App carries the
		// lowest wave — it goes Healthy before every consumer App. Carving it last
		// (PR-C in the original plan) is why its negative test is the containment proof.
		CarvedApp: &CarvedApp{AppName: "llz-externalsecrets", AppWave: -10, Namespace: "external-secrets"},
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
			"applications/openbao.yaml",
		},
	},
	{
		// apl-core's monitoring stack + the llz glue (loki S3 ExternalSecret, alert
		// rules, the OTel collector serving-TLS CA chain) that rides with it.
		Name:        "observability",
		AplCoreApps: []string{"prometheus", "alertmanager", "grafana", "loki", "otel"},
		ManifestResources: []string{
			"observability/loki-object-store-externalsecret.yaml",
			"observability/otel-bootstrap-ca.yaml",
			"observability/otel-collector.yaml",
			"observability/prometheus-rules/openbao-alerts.yaml",
			"observability/prometheus-rules/support-plane-alerts.yaml",
		},
		// The env-shaped otel.<env>.internal SAN on the collector serving cert —
		// rendered per env by RenderOtelSANPatch (replaces spec.dnsNames wholesale).
		Patches: []Patch{{
			Path:    "otel-collector-tls-san-patch.yaml",
			Group:   "cert-manager.io",
			Version: "v1",
			Kind:    "Certificate",
			Name:    "platform-otel-collector-tls",
		}},
		// A leaf — nothing depends on observability's health — so it was the cheap
		// first carve (PR-A). Its content spans waves -16..10; the App wave floors
		// them and lands after externalSecrets.
		CarvedApp: &CarvedApp{AppName: "llz-observability", AppWave: -5, Namespace: "llz-observability"},
	},
	{
		Name:        "harbor",
		AplCoreApps: []string{"harbor"},
		ManifestResources: []string{
			"harbor/harbor-registry-s3-externalsecret.yaml",
			"harbor/harbor-admin-push.yaml",
			"harbor/harbor-robot-provisioner",
		},
		// The env-shaped HARBOR_HOST (registry host) on the robot-provisioner
		// CronJob — rendered per env by RenderHarborHostPatch.
		Patches: []Patch{{
			Path:    "harbor-provisioner-env-patch.yaml",
			Group:   "batch",
			Version: "v1",
			Kind:    "CronJob",
			Name:    "harbor-robot-provisioner",
		}},
		// All harbor content sits at wave 5 (after the openbao ClusterSecretStore);
		// its own App wave keeps the robot-provisioner CronJob + mesh NetworkPolicy
		// off platform-bootstrap's fate.
		CarvedApp: &CarvedApp{AppName: "llz-harbor", AppWave: 5, Namespace: "harbor"},
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
		// In-cluster Gitea — apl-core-only. Disabled by default on apl-core v6:
		// the landing zone runs GitOps against an external HTTPS repo (BYO Git),
		// v6 ships git-server as the default values-repo backend, and _shared
		// values.yaml pins `gitea: { enabled: false }`. Modeling it enabled-by-
		// default here would make `llz render` flip that committed `false` back to
		// `true` (RenderValues forces every default-enabled component's app on),
		// silently re-enabling Gitea. Kept in the registry (not deleted) so an
		// operator can still opt in via the spec, but DefaultDisabled.
		Name:            "gitea",
		AplCoreApps:     []string{"gitea"},
		DefaultDisabled: true,
	},
	{
		// Support glue for the Akamai-internal llz-linode-cidr-firewall
		// controller (docs/consume-lke-landing-zone-internal.md): the ESO-synced
		// kube-system/linode token Secret + the self-discovery CronJob that
		// reconciles the controller's ConfigMap (firewall ID / LKE cluster ID /
		// VPC CIDR) from the pod's own node — replacing the per-apply
		// `llz ci bootstrap-cloud-firewall` workflow seed. Default-disabled:
		// the controller chart itself is private; consumers who add it enable
		// this alongside. Needs ESO for the token Secret.
		Name:              "cidrFirewall",
		DependsOn:         []string{"externalSecrets"},
		ManifestResources: []string{"llz-cidr-firewall"},
		DefaultDisabled:   true,
	},
	{
		// In-cluster reconciler + convergence metrics surface (Phase 0:
		// observe-only). Deploys the long-lived `llz reconcile` process that
		// samples cluster signals and serves them at :8080/metrics, plus the
		// wiring that closes the Prometheus scrape path (Service, ServiceMonitor,
		// default-deny-compatible NetworkPolicy, read-only RBAC, alert rules).
		// DependsOn observability: the ServiceMonitor + PrometheusRule CRDs come
		// from kube-prometheus-stack, and there is no point publishing metrics no
		// Prometheus scrapes. See docs/designs/kube-native-reconciler.md.
		//
		// DEFAULT-ON (rollout batch 1): the Deployment runs observe-only + the two
		// zero-wiring driving reconcilers (argo-nudge, sc-demote — flags in the
		// component deployment.yaml). Both are RBAC-ready and idempotent alongside
		// the CronJobs they will eventually replace, so this is safe fleet-wide; the
		// Linode/OpenBao reconcilers stay off (their flags need per-env env/secrets).
		Name:              "llzReconciler",
		DependsOn:         []string{"observability"},
		ManifestResources: []string{"llz-reconciler"},
		// Per-env REGION_SHORT for the volume-labels reconciler (the one genuine
		// per-env delta; `llz render` emits llz-reconciler-env-patch.yaml).
		Patches: []Patch{{
			Path:    "llz-reconciler-env-patch.yaml",
			Group:   "apps",
			Version: "v1",
			Kind:    "Deployment",
			Name:    "llz-reconciler",
		}},
		// The reconciler Deployment (wave 6) consumes its own ExternalSecret (wave 5)
		// — the same in-bundle pair the #163 wedge was born from — so its App wave
		// (5) lands after externalSecrets/observability and floors both.
		CarvedApp: &CarvedApp{AppName: "llz-reconciler", AppWave: 5, Namespace: "llz-reconciler"},
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
// the map — or present with a nil Enabled (a tune-only toggle) — falls back to the
// component's built-in default (Defaults() fills the map, so this is mainly for
// renderers that receive a partial map).
func ComponentEnabled(toggles map[string]ComponentToggle, name string) bool {
	if t, ok := toggles[name]; ok && t.Enabled != nil {
		return *t.Enabled
	}
	c, ok := componentByName[name]
	return ok && !c.DefaultDisabled
}

// boolPtr returns a pointer to b (for tri-state ComponentToggle.Enabled defaults).
func boolPtr(b bool) *bool { return &b }

// ComponentKnobs returns the spec.components sizing fields a component reads
// (empty for components with no capacity knobs). Exposed for `llz components`.
func ComponentKnobs(name string) []string { return sizingKnobs[name] }

// Backends returns the human-readable delivery backends a component routes to:
// "apl-core" (apps.<key>.enabled in values.yaml) and/or "llz-argo" (manifest /
// Argo Application resources). Empty for a marker-only component.
func (c Component) Backends() []string {
	var b []string
	if len(c.AplCoreApps) > 0 {
		b = append(b, "apl-core")
	}
	if len(c.ManifestResources) > 0 || len(c.ArgoApps) > 0 || len(c.Patches) > 0 {
		b = append(b, "llz-argo")
	}
	return b
}
