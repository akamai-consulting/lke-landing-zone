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
	targets, err := renderGoldenWith(t, func(lz *clusterspec.LandingZone) {
		lz.Spec.Alerting = clusterspec.Alerting{Receivers: []string{"pagerduty"}}
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
		if body != clusterspec.RenderAlertingManifest(clusterspec.Alerting{Receivers: []string{"pagerduty"}}) {
			t.Errorf("%s: rendered %s is not the renderer's output", env, clusterspec.AlertingFile)
		}
		k := targets[filepath.Join(dir, "kustomization.yaml")]
		if !strings.Contains(k, "resources:\n  - "+clusterspec.AlertingFile+"\n") {
			t.Errorf("%s: %s is rendered but the observability kustomization does not list it — Argo would never apply it:\n%s",
				env, clusterspec.AlertingFile, k)
		}
	}
}

// AN INSTANCE WITHOUT ALERTING SEES NO CHANGE: no file, no resources: block.
// (The golden pins the bytes; this pins the reason.)
func TestNoAlertingRendersNoAlertingFile(t *testing.T) {
	targets, err := renderGoldenWith(t, func(lz *clusterspec.LandingZone) { lz.Spec.Alerting = clusterspec.Alerting{} })
	if err != nil {
		t.Fatal(err)
	}
	for p, body := range targets {
		if strings.HasSuffix(p, clusterspec.AlertingFile) {
			t.Errorf("rendered %s with no spec.alerting", p)
		}
		if strings.HasSuffix(p, filepath.Join("observability", "kustomization.yaml")) && strings.Contains(body, "resources:") {
			t.Errorf("%s gained a resources: block with no spec.alerting — every instance would see drift on upgrade", p)
		}
	}
}

// A pager the spec asks for and an env cannot carry is a render ERROR, never a
// skip — the skip is the silence spec.alerting lived in for the whole managed era.
func TestSpecAlertingWithoutObservabilityIsARenderError(t *testing.T) {
	off := false
	_, err := renderGoldenWith(t, func(lz *clusterspec.LandingZone) {
		lz.Spec.Alerting = clusterspec.Alerting{Receivers: []string{"pagerduty"}}
		e := lz.Spec.Environments["staging"]
		if e.Components == nil {
			e.Components = map[string]clusterspec.ComponentToggle{}
		}
		e.Components["observability"] = clusterspec.ComponentToggle{Enabled: &off}
		lz.Spec.Environments["staging"] = e
	})
	if err == nil || !strings.Contains(err.Error(), "observability") {
		t.Fatalf("an env without observability rendered spec.alerting silently: err=%v", err)
	}
}
