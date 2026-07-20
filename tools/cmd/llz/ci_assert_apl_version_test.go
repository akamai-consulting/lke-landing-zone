package main

import (
	"strings"
	"testing"
)

// The guard must pass every supported pin and reject exactly the ones that wedge a
// bootstrap — with an error that names the two v6-only assumptions, so an operator
// reading CI output knows what breaks and how to fix it.
func TestAplVersionSupported(t *testing.T) {
	for _, v := range []string{minSupportedAplChartVersion, "6.0.1", "6.1.2", "7.0.0"} {
		if err := aplVersionSupported(v, "prod"); err != nil {
			t.Errorf("%s should be supported, got: %v", v, err)
		}
	}

	// 5.x is the live failure this guard exists for (prod ran apl-core v5.0.0 under a
	// v6-only template): apl-operator wedged on the dropped apl-sops-secrets
	// placeholder and the cluster got no ESO at all.
	for _, v := range []string{"5.0.0", "5.9.9", "0.1.0"} {
		err := aplVersionSupported(v, "prod")
		if err == nil {
			t.Fatalf("%s must be rejected (older than %s)", v, minSupportedAplChartVersion)
		}
		msg := err.Error()
		for _, want := range []string{"apl-sops-secrets", "external-secrets", minSupportedAplChartVersion, v} {
			if !strings.Contains(msg, want) {
				t.Errorf("rejection of %s should mention %q; got:\n%s", v, want, msg)
			}
		}
		// The import path pins 5.0.0, so the message must call that out — it is how an
		// imported instance reaches this state by default.
		if !strings.Contains(msg, importInitAplChartVersion) {
			t.Errorf("rejection should name the import-init pin %s; got:\n%s", importInitAplChartVersion, msg)
		}
	}

	// A non-semver pin is a configuration error, not a silent pass.
	err := aplVersionSupported("main", "prod")
	if err == nil || !strings.Contains(err.Error(), "not a semver") {
		t.Errorf("unparseable version must fail with a semver error, got: %v", err)
	}
}

// The default the template bakes must itself satisfy the guard — otherwise every
// instance without an explicit pin would fail this preflight.
func TestDefaultAplChartVersionIsSupported(t *testing.T) {
	if err := aplVersionSupported(defaultAplChartVersion, "prod"); err != nil {
		t.Fatalf("defaultAplChartVersion %s must satisfy the guard: %v", defaultAplChartVersion, err)
	}
}
