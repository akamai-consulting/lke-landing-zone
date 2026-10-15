package render

// spec.alerting LEFT clusterspec.InertSpecFields when it was wired, so the
// ratchet there no longer watches it. This is its replacement, the positive
// half: set the field, run the REAL render, and require the AlertmanagerConfig
// to be both written AND referenced — a file Argo never reads is the same
// silence as no file.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

func renderGoldenWith(t *testing.T, mutate func(*clusterspec.LandingZone)) (map[string]string, error) {
	t.Helper()
	t.Setenv("LLZ_TEMPLATE_REF", "v0.0.0-golden")
	lz, err := clusterspec.Decode([]byte(goldenSpec))
	if err != nil {
		t.Fatalf("decode goldenSpec: %v", err)
	}
	mutate(lz)
	const instRoot = "/inst"
	return renderTargets(lz, []string{"prod", "staging"},
		filepath.Join(instRoot, "terraform-iac-bootstrap"),
		filepath.Join(instRoot, "apl-values"), false)
}

func TestSpecAlertingRendersIntoTheObservabilityApp(t *testing.T) {
	a := clusterspec.Alerting{Receivers: []string{"pagerduty"}}
	targets, err := renderGoldenWith(t, func(lz *clusterspec.LandingZone) {
		lz.Spec.Alerting = a
		for name, e := range lz.Spec.Environments {
			e.Alerting = nil
			lz.Spec.Environments[name] = e
		}
	})
	if err != nil {
		t.Fatalf("renderTargets: %v", err)
	}
	for _, env := range []string{"prod", "staging"} {
		dir := filepath.Join("/inst", "apl-values", env, "apps", "observability")
		body, ok := targets[filepath.Join(dir, clusterspec.AlertingFile)]
		if !ok {
			t.Fatalf("%s: spec.alerting set, but no %s rendered", env, clusterspec.AlertingFile)
		}
		if body != clusterspec.RenderAlertingManifest(a) {
			t.Errorf("%s: rendered %s is not the renderer's output", env, clusterspec.AlertingFile)
		}
		k := targets[filepath.Join(dir, "kustomization.yaml")]
		if !strings.Contains(k, "resources:\n  - "+clusterspec.AlertingFile+"\n") {
			t.Errorf("%s: %s is rendered but the observability kustomization does not list it — Argo would never apply it:\n%s",
				env, clusterspec.AlertingFile, k)
		}
		// The apl-core half rides the apl-overlay — and with pagerduty only it
		// pins apl-core's own receivers to [none], so no unspecified Slack path
		// survives a Console edit.
		as, ok := targets[filepath.Join("/inst", "apl-values", env, "apl-overlay", clusterspec.OverlayAlertsFile)]
		if !ok || as != clusterspec.RenderAlertSetOverlay(a) {
			t.Errorf("%s: the AplAlertSet overlay is missing or not the renderer's output:\n%s", env, as)
		}
	}
}

// A PER-ENV OVERRIDE REACHES ONLY ITS ENV. staging opts out of the instance's
// pager; prod keeps it.
func TestAnEnvOverrideRendersOnlyIntoItsEnv(t *testing.T) {
	targets, err := renderGoldenWith(t, func(lz *clusterspec.LandingZone) {
		lz.Spec.Alerting = clusterspec.Alerting{Receivers: []string{"pagerduty"}}
		e := lz.Spec.Environments["staging"]
		e.Alerting = &clusterspec.Alerting{Receivers: []string{"none"}}
		lz.Spec.Environments["staging"] = e
	})
	if err != nil {
		t.Fatal(err)
	}
	obs := func(env string) string { return filepath.Join("/inst", "apl-values", env, "apps", "observability") }
	if _, ok := targets[filepath.Join(obs("prod"), clusterspec.AlertingFile)]; !ok {
		t.Error("prod lost the instance-wide pager")
	}
	if _, ok := targets[filepath.Join(obs("staging"), clusterspec.AlertingFile)]; ok {
		t.Error("staging opted out with receivers [none] and still rendered an AlertmanagerConfig — dev pages like prod")
	}
	if strings.Contains(targets[filepath.Join(obs("staging"), "kustomization.yaml")], clusterspec.AlertingFile) {
		t.Error("staging's observability kustomization still lists alerting.yaml")
	}
	as := targets[filepath.Join("/inst", "apl-values", "staging", "apl-overlay", clusterspec.OverlayAlertsFile)]
	if !strings.Contains(as, "- none") {
		t.Errorf("staging's AplAlertSet must assert [none]:\n%s", as)
	}
}

// AN INSTANCE WITHOUT ALERTING SEES NO CHANGE: no file, no resources: block, no
// AplAlertSet. (The golden pins the bytes; this pins the reason.)
func TestNoAlertingRendersNoAlertingFile(t *testing.T) {
	targets, err := renderGoldenWith(t, func(lz *clusterspec.LandingZone) {
		lz.Spec.Alerting = clusterspec.Alerting{}
		for name, e := range lz.Spec.Environments {
			e.Alerting = nil
			lz.Spec.Environments[name] = e
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, body := range targets {
		if strings.HasSuffix(p, clusterspec.AlertingFile) || strings.HasSuffix(p, filepath.Join("apl-overlay", clusterspec.OverlayAlertsFile)) {
			t.Errorf("rendered %s with no spec.alerting", p)
		}
		if strings.HasSuffix(p, filepath.Join("observability", "kustomization.yaml")) && strings.Contains(body, "resources:") {
			t.Errorf("%s gained a resources: block with no spec.alerting — every instance would see drift on upgrade", p)
		}
	}
}

func withoutObservability(lz *clusterspec.LandingZone, env string) {
	off := false
	e := lz.Spec.Environments[env]
	if e.Components == nil {
		e.Components = map[string]clusterspec.ComponentToggle{}
	}
	e.Components["observability"] = clusterspec.ComponentToggle{Enabled: &off}
	lz.Spec.Environments[env] = e
}

// AN ENV WITHOUT OBSERVABILITY IS SKIPPED, NOT FAILED, when it merely inherits an
// instance-wide receiver: it runs no Alertmanager, and an instance-wide setting
// must not make it unrenderable. Nothing is rendered for it — not the CR, not
// the AplAlertSet (whose Slack would have no secret beside it).
func TestAnEnvWithoutObservabilityIsSkipped(t *testing.T) {
	targets, err := renderGoldenWith(t, func(lz *clusterspec.LandingZone) {
		lz.Spec.Alerting = clusterspec.Alerting{Receivers: []string{"pagerduty", "slack"}}
		e := lz.Spec.Environments["staging"]
		e.Alerting = nil
		lz.Spec.Environments["staging"] = e
		withoutObservability(lz, "staging")
	})
	if err != nil {
		t.Fatalf("an env without observability failed the whole render: %v", err)
	}
	for p := range targets {
		if strings.Contains(p, filepath.Join("apl-values", "staging")) &&
			(strings.HasSuffix(p, clusterspec.AlertingFile) || strings.HasSuffix(p, clusterspec.OverlayAlertsFile)) {
			t.Errorf("staging deploys no observability but rendered %s", p)
		}
	}
	if _, ok := targets[filepath.Join("/inst", "apl-values", "prod", "apps", "observability", clusterspec.AlertingFile)]; !ok {
		t.Error("prod's alerting stopped rendering because staging has no observability")
	}
}

// …BUT AN ENV THAT ASKS FOR A RECEIVER ITSELF, without observability, is a
// validation error: the author said something LLZ would silently not do.
func TestAnEnvAskingForAReceiverWithoutObservabilityFailsValidation(t *testing.T) {
	lz, err := clusterspec.Decode([]byte(goldenSpec))
	if err != nil {
		t.Fatal(err)
	}
	withoutObservability(lz, "staging")
	found := false
	for _, e := range lz.Validate() {
		if strings.Contains(e.Error(), "environments/staging: alerting") && strings.Contains(e.Error(), "observability") {
			found = true
		}
	}
	if !found {
		t.Errorf("staging names slack itself without observability, and Validate said nothing: %v", lz.Validate())
	}
	// Inherited only → no such error.
	e := lz.Spec.Environments["staging"]
	e.Alerting = nil
	lz.Spec.Environments["staging"] = e
	for _, err := range lz.Validate() {
		if strings.Contains(err.Error(), "alerting") {
			t.Errorf("an env merely INHERITING alerting without observability must be skipped, not rejected: %v", err)
		}
	}
}
