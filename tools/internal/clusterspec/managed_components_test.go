package clusterspec

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// comp looks a registry component up by name for the disposition tests.
func comp(t *testing.T, name string) Component {
	t.Helper()
	for _, c := range Components {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("component %q not in the registry", name)
	return Component{}
}

func TestBootstrapManagedAppEnabled(t *testing.T) {
	b := Bootstrap{ManagedApps: []string{"harbor", "loki"}}
	if !b.ManagedAppEnabled("harbor") || !b.ManagedAppEnabled("loki") {
		t.Error("declared apps should be enabled")
	}
	if b.ManagedAppEnabled("grafana") {
		t.Error("undeclared app must not be enabled")
	}
	if (Bootstrap{}).ManagedAppEnabled("harbor") {
		t.Error("empty managedApps → nothing enabled")
	}
}

// TestEmitOnManaged: ManagedSkip components never emit; components conditional on
// an apl-core app emit only when it is declared; components conditional on a
// sibling LLZ component emit only when that consumer is enabled; everything else
// emits.
func TestEmitOnManaged(t *testing.T) {
	withHarbor := Bootstrap{ManagedApps: []string{"harbor"}}
	none := Bootstrap{}

	// Skip components (apl-core owns them on managed) never emit.
	for _, name := range []string{"clusterFoundation", "argoEvents", "gitea", "policyEngine", "imageScanning"} {
		if comp(t, name).EmitOnManaged(withHarbor, nil) {
			t.Errorf("%s (ManagedSkip) must never emit on managed", name)
		}
	}
	// Always-on LLZ components emit regardless of declared apps.
	for _, name := range []string{"openbao", "externalSecrets", "certManagerBootstrapCA", "llzReconciler", "broadPatRotator"} {
		if !comp(t, name).EmitOnManaged(none, nil) {
			t.Errorf("%s (always) must emit on managed", name)
		}
	}
	// Conditional components gate on the declared apl-core app.
	if !comp(t, "harbor").EmitOnManaged(withHarbor, nil) {
		t.Error("harbor must emit when harbor is declared")
	}
	if comp(t, "harbor").EmitOnManaged(none, nil) {
		t.Error("harbor must NOT emit when harbor is not declared")
	}
	if comp(t, "observability").EmitOnManaged(withHarbor, nil) {
		t.Error("observability (conditional on loki) must NOT emit when only harbor declared")
	}
	if !comp(t, "observability").EmitOnManaged(Bootstrap{ManagedApps: []string{"loki"}}, nil) {
		t.Error("observability must emit when loki is declared")
	}

	// Consumer-gated components (argoWorkflows) emit on managed only when their
	// consumer (clusterHealthWorkflow) is enabled — not on a default cluster.
	chwOn := map[string]ComponentToggle{"clusterHealthWorkflow": {Enabled: boolPtr(true)}}
	if comp(t, "argoWorkflows").EmitOnManaged(none, nil) {
		t.Error("argoWorkflows must NOT emit on managed when clusterHealthWorkflow is disabled (default)")
	}
	if !comp(t, "argoWorkflows").EmitOnManaged(none, chwOn) {
		t.Error("argoWorkflows must emit on managed when clusterHealthWorkflow is enabled")
	}
	// clusterHealthWorkflow is no longer ManagedSkip: enabled → emits.
	if !comp(t, "clusterHealthWorkflow").EmitOnManaged(none, chwOn) {
		t.Error("clusterHealthWorkflow must emit on managed when enabled")
	}

	// …but the consumer gate is a DEFAULT, not the only door. An operator who wants
	// the Workflow CRDs for their own build pipeline can say so directly, instead of
	// enabling an unrelated-sounding health component for its DependsOn side effect.
	awOn := map[string]ComponentToggle{"argoWorkflows": {Enabled: boolPtr(true), Explicit: true}}
	if !comp(t, "argoWorkflows").EmitOnManaged(none, awOn) {
		t.Error("argoWorkflows must emit on managed when the operator explicitly enables it")
	}
	// EXPLICIT means the AUTHOR wrote it. argoWorkflows is default-enabled, so a
	// toggle Defaults() merely filled in must NOT open the gate — otherwise every
	// managed cluster gets argo-workflows and the gate is dead.
	for name, toggles := range map[string]map[string]ComponentToggle{
		"defaulted-on (Explicit unset)": {"argoWorkflows": {Enabled: boolPtr(true)}},
		"sizing knob only":              {"argoWorkflows": {Storage: "10Gi"}},
		"explicit false":                {"argoWorkflows": {Enabled: boolPtr(false), Explicit: true}},
	} {
		if comp(t, "argoWorkflows").EmitOnManaged(none, toggles) {
			t.Errorf("argoWorkflows must NOT emit on managed for %s — only an author-written enabled:true", name)
		}
	}
}

// TestArgoWorkflowsOptInThroughDefaults walks the REAL path — parse → merge →
// Defaults → EmitOnManaged — because that is where the earlier attempt at this
// broke: Defaults() materializes a complete toggle map, so after it runs every
// component looks explicitly enabled unless the "author wrote it" bit is carried
// forward. Hand-built toggle maps in the sibling test cannot catch that.
func TestArgoWorkflowsOptInThroughDefaults(t *testing.T) {
	emits := func(t *testing.T, componentsYAML string) bool {
		t.Helper()
		lz := &LandingZone{}
		lz.Spec.Environments = map[string]Environment{"prod": {
			Cluster: Cluster{Bootstrap: Bootstrap{ManagedAppPlatform: true}},
		}}
		if componentsYAML != "" {
			var toggles map[string]ComponentToggle
			if err := yaml.UnmarshalStrict([]byte(componentsYAML), &toggles); err != nil {
				t.Fatal(err)
			}
			e := lz.Spec.Environments["prod"]
			e.Components = toggles
			lz.Spec.Environments["prod"] = e
		}
		lz.Defaults()
		e := lz.Spec.Environments["prod"]
		return comp(t, "argoWorkflows").EmitOnManaged(e.Cluster.Bootstrap, e.Components)
	}

	// The default managed cluster: no argo-workflows, exactly as before.
	if emits(t, "") {
		t.Error("a spec that says nothing must NOT get argoWorkflows on managed")
	}
	// Unrelated toggles must not drag it in — this is the case that regressed.
	if emits(t, "observability: {retention: 30d}\n") {
		t.Error("an unrelated toggle must not enable argoWorkflows on managed")
	}
	// The new door: ask for it by name.
	if !emits(t, "argoWorkflows: {enabled: true}\n") {
		t.Error("components.argoWorkflows.enabled=true must emit argoWorkflows on managed")
	}
	// The old door still works.
	if !emits(t, "clusterHealthWorkflow: {enabled: true}\n") {
		t.Error("the clusterHealthWorkflow consumer gate must still emit argoWorkflows")
	}
}

// TestValidateEnv_ManagedCrossFields: managedAppPlatform is required true, no
// domainSuffix, managedApps must be well-formed, and the removed certManager /
// certAutomation components get an actionable migration message.
func TestValidateEnv_ManagedCrossFields(t *testing.T) {
	hasErr := func(errs []error, sub string) bool {
		for _, e := range errs {
			if strings.Contains(e.Error(), sub) {
				return true
			}
		}
		return false
	}
	mk := func(b Bootstrap, comps map[string]ComponentToggle) Environment {
		return Environment{Cluster: Cluster{Bootstrap: b}, Components: comps}
	}

	if !hasErr(validateEnv("m", mk(Bootstrap{ManagedAppPlatform: false}, nil)), "managedAppPlatform must be true") {
		t.Error("a non-managed spec must be rejected — LLZ never self-installs apl-core")
	}
	if !hasErr(validateEnv("m", mk(Bootstrap{ManagedAppPlatform: true, DomainSuffix: "web.example.com"}, nil)), "domainSuffix must NOT be set") {
		t.Error("domainSuffix with managedAppPlatform must be rejected")
	}
	if !hasErr(validateEnv("m", mk(Bootstrap{ManagedAppPlatform: true, ManagedApps: []string{"Harbor"}}, nil)), "managedApps entry") {
		t.Error("a malformed managedApps entry (uppercase) must be rejected")
	}
	errs := validateEnv("m", mk(Bootstrap{ManagedAppPlatform: true, ManagedApps: []string{"harbor", "loki"}}, nil))
	if hasErr(errs, "managedApps entry") || hasErr(errs, "domainSuffix must NOT") || hasErr(errs, "managedAppPlatform must be true") {
		t.Errorf("valid managed cross-fields should not error: %v", errs)
	}
	for _, stale := range []string{"certManager", "certAutomation"} {
		if !hasErr(validateEnv("m", mk(Bootstrap{ManagedAppPlatform: true}, map[string]ComponentToggle{stale: {}})), "no longer exists") {
			t.Errorf("a stale components.%s must get the migration message", stale)
		}
	}
}
