package clusterspec

import (
	"strings"
	"testing"
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

// TestEmitOnManaged: ManagedSkip components never emit; conditional components
// emit only when their gating apl-core app is declared; everything else emits.
func TestEmitOnManaged(t *testing.T) {
	withHarbor := Bootstrap{ManagedApps: []string{"harbor"}}
	none := Bootstrap{}

	// Skip components (apl-core owns them on managed) never emit.
	for _, name := range []string{"clusterFoundation", "argoWorkflows", "argoEvents", "gitea", "policyEngine", "imageScanning", "clusterHealthWorkflow"} {
		if comp(t, name).EmitOnManaged(withHarbor) {
			t.Errorf("%s (ManagedSkip) must never emit on managed", name)
		}
	}
	// Always-on LLZ components emit regardless of declared apps.
	for _, name := range []string{"openbao", "externalSecrets", "certManagerBootstrapCA", "llzReconciler", "broadPatRotator"} {
		if !comp(t, name).EmitOnManaged(none) {
			t.Errorf("%s (always) must emit on managed", name)
		}
	}
	// Conditional components gate on the declared apl-core app.
	if !comp(t, "harbor").EmitOnManaged(withHarbor) {
		t.Error("harbor must emit when harbor is declared")
	}
	if comp(t, "harbor").EmitOnManaged(none) {
		t.Error("harbor must NOT emit when harbor is not declared")
	}
	if comp(t, "observability").EmitOnManaged(withHarbor) {
		t.Error("observability (conditional on loki) must NOT emit when only harbor declared")
	}
	if !comp(t, "observability").EmitOnManaged(Bootstrap{ManagedApps: []string{"loki"}}) {
		t.Error("observability must emit when loki is declared")
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
