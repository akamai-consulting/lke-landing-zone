package clusterspec

import (
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// OFF BY DEFAULT MEANS NO FILE AT ALL. Linode installs and versions apl-core on
// managed; an instance that has not asked llz to own that must be left exactly as it
// is, and an empty render is what stops the file being written.
func TestOtomiOverlayIsOffUnlessAskedFor(t *testing.T) {
	if got := RenderOtomiOverlayEnv(Bootstrap{AplChartVersion: "v6.2.1"}); got != "" {
		t.Errorf("no manageAplVersion means no otomi.yaml, got:\n%s", got)
	}
}

// THE ONLY KEY WRITTEN IS THE ONE LLZ OWNS. spec.git lives in this same CR and is
// apl-core's BYO-Git wiring; the overlay reconciler deep-merges, so a fragment that
// emitted `git: {}` would take the platform's own git config out from under it.
func TestOtomiOverlayWritesOnlyTheVersion(t *testing.T) {
	out := RenderOtomiOverlayEnv(Bootstrap{ManageAplVersion: true, AplChartVersion: "v6.2.0"})
	if out == "" {
		t.Fatal("manageAplVersion must render the file")
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("the fragment must parse: %v\n%s", err, out)
	}
	if doc["kind"] != otomiKind {
		t.Errorf("kind = %v, want %s — apl-core stores env/settings/otomi.yaml as that CR", doc["kind"], otomiKind)
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta["name"] != "otomi" {
		t.Errorf("metadata.name = %v, want otomi", meta["name"])
	}
	spec, _ := doc["spec"].(map[string]any)
	if spec["version"] != "v6.2.0" {
		t.Errorf("spec.version = %v, want v6.2.0", spec["version"])
	}
	if len(spec) != 1 {
		t.Errorf("spec must carry ONLY version, got %v — anything else deep-merges over apl-core's own capabilities", spec)
	}
	for _, forbidden := range []string{"git", "isMultitenant", "hasExternalDNS", "nodeSelector"} {
		if _, ok := spec[forbidden]; ok {
			t.Errorf("spec.%s must not be emitted; it is the platform's, not llz's", forbidden)
		}
	}
}

// THE EFFECTIVE VERSION, so this composes with the pin-retiring sweep: an env whose
// pin llz set is retired falls back to the baseline, and the baseline is what gets
// rendered — the deployment keeps tracking the release either way.
func TestOtomiOverlayRendersTheEffectiveVersion(t *testing.T) {
	// The bare form an operator may legitimately write here is normalised to the one
	// apl-core's schema accepts — same version, spelling it will not reject.
	pinned := RenderOtomiOverlayEnv(Bootstrap{ManageAplVersion: true, AplChartVersion: "6.1.0"})
	if !strings.Contains(pinned, "version: v6.1.0") {
		t.Errorf("an explicit pin is what deploys, in apl-core's spelling:\n%s", pinned)
	}
	// A letter-leading name is already what the pattern permits and is not ours to
	// reinterpret.
	named := RenderOtomiOverlayEnv(Bootstrap{ManageAplVersion: true, AplChartVersion: "main"})
	if !strings.Contains(named, "version: main") {
		t.Errorf("a branch-style version passes through untouched:\n%s", named)
	}
	unpinned := RenderOtomiOverlayEnv(Bootstrap{ManageAplVersion: true})
	if !strings.Contains(unpinned, "version: "+BaselineAplChartVersion) {
		t.Errorf("an omitted pin deploys this release's baseline (%s):\n%s", BaselineAplChartVersion, unpinned)
	}
}

// The rendered value must satisfy apl-core's own schema pattern for otomi.version,
// or the operator rejects the values tree it was handed.
//
//	pattern: '(v[0-9]+.[0-9]+.[0-9]+|[a-zA-Z]+[a-zA-Z0-9-])'
func TestOtomiOverlayVersionMatchesAplCoreSchema(t *testing.T) {
	for _, pin := range []string{"", "v6.2.1", "6.1.0", "v6.3.0-rc.1"} {
		out := RenderOtomiOverlayEnv(Bootstrap{ManageAplVersion: true, AplChartVersion: pin})
		var doc struct {
			Spec struct {
				Version string `yaml:"version"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("pin %q: %v", pin, err)
		}
		if !aplCoreVersionPattern.MatchString(doc.Spec.Version) {
			t.Errorf("pin %q rendered %q, which apl-core's values schema would reject", pin, doc.Spec.Version)
		}
	}
}
