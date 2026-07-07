package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWaveHealthVAPMatchesGuard pins the llz-wave-health-guard ValidatingAdmissionPolicy
// (the admission-time twin) to the CI guard's Go source of truth. The whole point of
// the VAP is to reject the SAME wedge class `llz ci wave-health-guard` catches, on the
// out-of-band write paths CI can't see — so its inline CEL allowlists MUST equal
// waveHealthAllowedKinds + waveHealthAllowedNames. If they drift (a kind vetted in one
// place but not the other), the two guards disagree and this fails the build.
func TestWaveHealthVAPMatchesGuard(t *testing.T) {
	path := esRepoPath("../../..", "apl-values/_shared/manifest/admission/wave-health-policy.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read VAP: %v", err)
	}
	var vap struct {
		Spec struct {
			Variables []struct {
				Name       string `yaml:"name"`
				Expression string `yaml:"expression"`
			} `yaml:"variables"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &vap); err != nil {
		t.Fatalf("parse VAP: %v", err)
	}
	exprByVar := map[string]string{}
	for _, v := range vap.Spec.Variables {
		exprByVar[v.Name] = v.Expression
	}

	// The CEL list literals hold double-quoted "group/Kind" (and "group/Kind/name")
	// tokens; pull them out and compare as sets.
	tokenRe := regexp.MustCompile(`"([^"]+)"`)
	celSet := func(varName string) map[string]bool {
		out := map[string]bool{}
		for _, m := range tokenRe.FindAllStringSubmatch(exprByVar[varName], -1) {
			out[m[1]] = true
		}
		return out
	}

	assertSameSet(t, "allowedKinds", keySet(waveHealthAllowedKindsSet()), celSet("allowedKinds"))
	assertSameSet(t, "allowedNames", keySet(waveHealthAllowedNamesSet()), celSet("allowedNames"))
}

// TestWaveHealthVAPSkipsHooks pins the VAP's hook exclusion in lockstep with the CI
// guard (classifyWaveHealthDoc) + the runtime audit (auditNegativeWave), which both
// skip resources carrying argocd.argoproj.io/hook. If the VAP's matchCondition is
// dropped, the admission twin would deny a hook the other two allow (the release-e2e
// v0.0.23 coredns-restart PostSync Job false positive) — so fail the build.
func TestWaveHealthVAPSkipsHooks(t *testing.T) {
	path := esRepoPath("../../..", "apl-values/_shared/manifest/admission/wave-health-policy.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read VAP: %v", err)
	}
	var vap struct {
		Spec struct {
			MatchConditions []struct {
				Name       string `yaml:"name"`
				Expression string `yaml:"expression"`
			} `yaml:"matchConditions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &vap); err != nil {
		t.Fatalf("parse VAP: %v", err)
	}
	found := false
	for _, mc := range vap.Spec.MatchConditions {
		e := strings.ReplaceAll(mc.Expression, " ", "")
		if strings.Contains(e, "!('argocd.argoproj.io/hook'inobject.metadata.annotations)") {
			found = true
		}
	}
	if !found {
		t.Fatal("VAP must carry a matchCondition excluding Argo hooks " +
			"(!('argocd.argoproj.io/hook' in object.metadata.annotations)) — in lockstep " +
			"with the CI guard + runtime audit hook-skip")
	}
}

// waveHealthAllowedKindsSet / waveHealthAllowedNamesSet expose the guard maps' keys
// (the maps themselves are unexported values in ci_wave_health_guard.go).
func waveHealthAllowedKindsSet() map[string]waveHealthKindRule { return waveHealthAllowedKinds }
func waveHealthAllowedNamesSet() map[string]waveHealthKindRule { return waveHealthAllowedNames }

func keySet(m map[string]waveHealthKindRule) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func assertSameSet(t *testing.T, label string, want, got map[string]bool) {
	t.Helper()
	var missing, extra []string
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%s: in the Go guard but MISSING from the VAP CEL allowlist: %s\n"+
			"add them to instance-template/apl-values/_shared/manifest/admission/wave-health-policy.yaml",
			label, strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		t.Errorf("%s: in the VAP CEL allowlist but NOT in the Go guard (waveHealthAllowed*): %s\n"+
			"add them to ci_wave_health_guard.go or remove them from the VAP",
			label, strings.Join(extra, ", "))
	}
}
