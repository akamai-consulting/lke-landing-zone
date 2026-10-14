package clusterspec

import (
	"strings"
	"testing"
)

// Tests closing the gaps a gremlins mutation run surfaced in this package. Each
// one names the mutant it kills, because the assertion that kills a mutant is
// often NOT the obvious assertion — see the drift-direction and both-set cases
// below, where the pre-existing tests covered the line but not the distinction.

func intPtrForMutants(i int) *int { return &i }

// merge.go:92 — `base == nil && over == nil` mutated to `||` survived: nothing
// exercised exactly-one-side-nil. That is the case the doc comment is about
// (nil means "nobody set anything, let Defaults() apply the full set"), so an
// `||` here would silently drop a side that WAS set.
func TestMergeComponents_OneSideNil(t *testing.T) {
	set := map[string]ComponentToggle{"harbor": {Enabled: boolPtr(true)}}

	if got := mergeComponents(nil, nil); got != nil {
		t.Errorf("both sides nil must stay nil so Defaults() applies, got %v", got)
	}
	got := mergeComponents(nil, set)
	if got == nil {
		t.Fatal("base nil + over set must keep the over side, got nil")
	}
	if e := got["harbor"].Enabled; e == nil || !*e {
		t.Errorf("base nil + over set lost the env toggle: %+v", got["harbor"])
	}
	got = mergeComponents(set, nil)
	if got == nil {
		t.Fatal("base set + over nil must keep the base side, got nil")
	}
	if e := got["harbor"].Enabled; e == nil || !*e {
		t.Errorf("base set + over nil lost the shared toggle: %+v", got["harbor"])
	}
}

// validate.go:578 — both `== ""` negations survived. The existing coverage had
// broadPatRotator either fully unset or absent, never half-configured, and
// never fully configured with the error asserted ABSENT. The both-set case is
// what kills each negation; the one-sided cases are the real defect (a spec
// with only one of the pair renders BROAD_PAT_* with an empty string, and the
// CronJob then fails at runtime).
func TestValidateComponents_BroadPATPartialConfig(t *testing.T) {
	const wantMsg = "broadPatRotator requires broadPATLabel and broadPATDeployments"

	for _, tc := range []struct {
		name        string
		label, deps string
		wantErr     bool
	}{
		{"both set", "llz-broad", "primary secondary", false},
		{"label only", "llz-broad", "", true},
		{"deployments only", "", "primary secondary", true},
		{"neither", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			components := allOn()
			components["broadPatRotator"] = ComponentToggle{
				Enabled:             boolPtr(true),
				BroadPATLabel:       tc.label,
				BroadPATDeployments: tc.deps,
			}
			var found bool
			for _, err := range validateComponents("dev", components) {
				if strings.Contains(err.Error(), wantMsg) {
					found = true
				}
			}
			if found != tc.wantErr {
				t.Errorf("label=%q deployments=%q: got error=%v, want error=%v", tc.label, tc.deps, found, tc.wantErr)
			}
		})
	}
}

// derived_values.go:90 — `TrimSpace(value) != value` mutated to `==` survived:
// the whitespace check on UNDECLARED env values was never exercised. Declared
// values are covered by their shape rules; this is the fallback for everything
// else, and inverted it would reject every clean value instead.
func TestCheckDerivedEnvValues_UndeclaredWhitespace(t *testing.T) {
	body := func(v string) string {
		return "        - name: LLZ_UNDECLARED_KNOB\n          value: \"" + v + "\"\n"
	}

	errs := CheckDerivedEnvValues(body("acme/instance "))
	if len(errs) != 1 {
		t.Fatalf("a trailing space on an undeclared value must be one error, got %v", errs)
	}
	if !strings.Contains(errs[0].Error(), "whitespace") {
		t.Errorf("error should name the whitespace damage, got: %v", errs[0])
	}
	if errs := CheckDerivedEnvValues(body(" acme/instance")); len(errs) != 1 {
		t.Errorf("a leading space must also be caught, got %v", errs)
	}
	if errs := CheckDerivedEnvValues(body("acme/instance")); len(errs) != 0 {
		t.Errorf("a clean undeclared value must pass, got %v", errs)
	}
}

// validate.go:628 — `*t.Replicas < 1` mutated to `<= 1` survived: replicas: 1
// was never asserted VALID, only replicas: 0 asserted invalid. The boundary is
// the whole point of the check.
func TestValidateComponentSizing_ReplicasBoundary(t *testing.T) {
	for _, tc := range []struct {
		replicas int
		wantErr  bool
	}{
		{-1, true},
		{0, true},
		{1, false}, // the boundary the mutant moved
		{3, false},
	} {
		errs := validateComponentSizing("dev", "observability", ComponentToggle{Replicas: intPtrForMutants(tc.replicas)})
		var found bool
		for _, err := range errs {
			if strings.Contains(err.Error(), "replicas must be >= 1") {
				found = true
			}
		}
		if found != tc.wantErr {
			t.Errorf("replicas=%d: got error=%v, want error=%v (errs: %v)", tc.replicas, found, tc.wantErr, errs)
		}
	}
}

// kustomize.go:74 — three mutants survived on
// `len(ManifestResources) > 0 || len(ArgoApps) > 0`: no fixture had a component
// with ArgoApps but no ManifestResources, so the second half of the disjunction
// was never load-bearing.
func TestHasManifestDir(t *testing.T) {
	for _, tc := range []struct {
		name      string
		component Component
		want      bool
	}{
		{"neither", Component{Name: "x"}, false},
		{"manifest resources only", Component{Name: "x", ManifestResources: []string{"deploy.yaml"}}, true},
		{"argo apps only", Component{Name: "x", ArgoApps: []string{"app.yaml"}}, true},
		{"both", Component{Name: "x", ManifestResources: []string{"deploy.yaml"}, ArgoApps: []string{"app.yaml"}}, true},
	} {
		if got := tc.component.hasManifestDir(); got != tc.want {
			t.Errorf("%s: hasManifestDir() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// kustomize.go:138 — `len(dirs) > 0` mutated to `>= 0` survived: every render
// under test had at least one plain component dir. With everything disabled the
// mutant emits a bare `components:` key with nothing under it, which is a
// malformed kustomization rather than a cosmetic difference.
func TestRenderManifestKustomization_NoComponentDirs(t *testing.T) {
	off := map[string]ComponentToggle{}
	for _, c := range Components {
		off[c.Name] = ComponentToggle{Enabled: boolPtr(false)}
	}
	out := RenderManifestKustomization(off, "", managedBoot())
	if strings.Contains(out, "components:") {
		t.Errorf("no enabled plain components must emit no components: key:\n%s", out)
	}
	// The base must still be there — this asserts the render is real, not empty.
	if !strings.Contains(out, "../../../../platform-apl/manifest") {
		t.Errorf("the shared base must survive an all-off render:\n%s", out)
	}
}

// aplversion.go:130 — `drift == AplChartDriftMajorBehind` mutated to `!=`
// survived even though BOTH directions had tests. The reason is worth keeping:
// the major-behind test asserted on env/pin/baseline/env-var, and the
// major-AHEAD message contains all four of those too, so the mutant just
// swapped one error for another that satisfied every assertion. Only the
// distinguishing prose separates them.
func TestAplChartVersionError_DistinguishesDriftDirection(t *testing.T) {
	behind := aplChartVersionError("prod", "5.0.0")
	if behind == nil {
		t.Fatal("a major-behind pin must block")
	}
	if !strings.Contains(behind.Error(), "would keep deploying APL") {
		t.Errorf("major-behind must explain the silent-stale case, got: %v", behind)
	}
	if strings.Contains(behind.Error(), "has not been tested against") {
		t.Errorf("major-behind must not report the major-ahead reason, got: %v", behind)
	}

	ahead := aplChartVersionError("dev", "7.0.0")
	if ahead == nil {
		t.Fatal("a major-ahead pin must block")
	}
	if !strings.Contains(ahead.Error(), "has not been tested against") {
		t.Errorf("major-ahead must explain the untested case, got: %v", ahead)
	}
	if strings.Contains(ahead.Error(), "would keep deploying APL") {
		t.Errorf("major-ahead must not report the major-behind reason, got: %v", ahead)
	}
}
