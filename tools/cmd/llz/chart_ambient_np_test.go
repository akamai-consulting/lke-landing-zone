package main

// chart_ambient_np_test.go asserts the Istio-ambient NetworkPolicy block in
// llz-cluster-foundation actually renders.
//
// WHY THIS IS A GO TEST AND NOT A MAKE RECIPE. The block ships behind
// `networkPolicies.ambient.enabled`, default FALSE, because the allows are
// written from the Istio docs against a data plane this platform does not run
// yet (see the values.yaml rationale). A default-off block is invisible to every
// existing gate: `make render-charts` materializes DEFAULTS, and kube-linter and
// kubeconform only ever see what was materialized. So the one path that matters
// here — the chart with the flag ON — is the one path nothing renders, and a
// break in it would stay green everywhere until the migration turned it on.
//
// The first version of this check was inline shell in the helm-lint-charts
// recipe. `llz ci untestable-loc` rejected it, correctly: that gate exists to
// keep decision logic out of CI shell and in unit-tested Go, and its budgets are
// a ratchet that must not be raised to make a red build pass. This is the same
// assertion in the place the repo wants it.

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ambientChartPath is the chart under test, relative to this package's dir.
const ambientChartPath = "../../../kubernetes-charts/llz-cluster-foundation"

// helmTemplateAmbient renders the foundation chart with ambient enabled.
func helmTemplateAmbient(t *testing.T, enabled string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil { // cheap proxy for "real checkout"
		t.Skip("not a usable checkout")
	}
	out, err := exec.Command("helm", "template", "llz-cluster-foundation",
		filepath.FromSlash(ambientChartPath),
		"--set", "networkPolicies.ambient.enabled="+enabled).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template (ambient=%s) failed: %v\n%s", enabled, err, out)
	}
	return string(out)
}

// With the flag on, every namespace baselineNP owns gets an ambient policy
// carrying both the HBONE port and the health-probe sources. Stubbing the
// template's condition makes this fail.
func TestAmbientNetworkPoliciesRender(t *testing.T) {
	out := helmTemplateAmbient(t, "true")
	for _, ns := range []string{"cert-manager", "harbor", "istio-system", "observability"} {
		if !strings.Contains(out, "name: "+ns+"-allow-ambient") {
			t.Errorf("no ambient NetworkPolicy for %s", ns)
		}
	}
	// The HBONE port is the whole point: without it, inbound to an enrolled pod
	// is dropped by the default-deny and every meshed hop fails.
	if !strings.Contains(out, "port: 15008") {
		t.Error("ambient render lost the HBONE port")
	}
	// Both families — the address used follows the POD's IP family, so a
	// single-stack assumption here CrashLoops probed pods on a dual-stack pod.
	for _, cidr := range []string{"169.254.7.127/32", "fd16:9254:7127:1337:ffff:ffff:ffff:ffff/128"} {
		if !strings.Contains(out, cidr) {
			t.Errorf("ambient render lost the health-probe source %s", cidr)
		}
	}
}

// OFF is the shipped default, and it must grant nothing. If this ever fails, the
// chart started widening ingress on every managed namespace for a data plane the
// platform does not run — the exact outcome the default-off decision avoids.
func TestAmbientNetworkPoliciesAbsentByDefault(t *testing.T) {
	out := helmTemplateAmbient(t, "false")
	if strings.Contains(out, "allow-ambient") {
		t.Error("ambient NetworkPolicies rendered with the flag off — they must ship disabled")
	}
	if strings.Contains(out, "15008") {
		t.Error("HBONE port present with ambient disabled")
	}
}
