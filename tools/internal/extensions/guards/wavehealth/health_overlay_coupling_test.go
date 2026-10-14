package wavehealth

// THE SPLIT CONTRACT, AND WHY IT NEEDED ITS OWN TEST.
//
// This guard holds one half of a rule: "these kinds are safe at a negative sync
// wave BECAUSE an Argo CD health override neutralizes their built-in check". The
// other half — the overrides themselves — lives in clusterspec, is rendered into
// the apl-overlay, and is merged onto apl-core's argocd AplApp CR by the
// in-cluster reconciler. Each half had a passing test over its own copy of the
// rule, which is exactly the arrangement that let the two drift for months: the
// guard cross-checked a file (apl-values/values.yaml) that `llz render` had
// stopped emitting, so it was green over a protection no cluster had.
//
// So this feeds the PRODUCER'S REAL OUTPUT into the CONSUMER'S REAL PREDICATE.
// Not a fixture that mirrors the overrides; not a restatement of the key names.
// If either side is edited alone, this fails.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// TestEveryOverrideBackedKindIsBackedByTheRenderedOverlay walks AllowedKinds and
// puts each override-backed kind through the guard's OWN classifier against the
// renderer's OWN output.
//
// DERIVED FROM AllowedKinds, NEVER LISTED. Naming the five keys by hand would
// pass unchanged the day a sixth kind gains an overrideKey, which is precisely
// how the drift this replaces arrived.
func TestEveryOverrideBackedKindIsBackedByTheRenderedOverlay(t *testing.T) {
	rendered := clusterspec.RenderAppValuesOverlayShared()
	checked := 0
	for groupKind, rule := range AllowedKinds {
		if rule.overrideKey == "" {
			continue
		}
		checked++
		f, ok := classifyWaveHealthDoc("probe.yaml", negativeWaveDoc(groupKind), rendered)
		if !ok {
			t.Errorf("%s: the classifier declined to judge a negative-wave doc of this kind — "+
				"this coupling test is not reaching the code it claims to test", groupKind)
			continue
		}
		if !f.allowed {
			t.Errorf("%s is allowed at a negative sync wave ONLY because of the %q health override, "+
				"and the guard's own classifier does not find that key in the rendered apl-overlay. "+
				"Either add it to argoHealthCustomizations() in clusterspec/overlay_appvalues.go, or "+
				"drop the kind from AllowedKinds — as it stands this guard vouches for a protection "+
				"no cluster has.", groupKind, rule.overrideKey)
		}
	}
	if checked == 0 {
		t.Fatal("no override-backed kinds were checked — AllowedKinds no longer has any, or the " +
			"overrideKey field moved, and this coupling test is passing vacuously")
	}
}

// THE NEGATIVE CONTROL. Without it the test above passes if `allowed` is stuck
// true for every input, which is the one bug that would make it useless.
func TestAnOverrideBackedKindIsRejectedWhenTheOverlayLacksItsKey(t *testing.T) {
	checked := 0
	for groupKind, rule := range AllowedKinds {
		if rule.overrideKey == "" {
			continue
		}
		checked++
		// The rendered overlay with THIS key removed, and only this key: every
		// sibling override stays, so a pass here would mean the classifier is not
		// reading the key it names.
		stripped := strings.ReplaceAll(clusterspec.RenderAppValuesOverlayShared(), rule.overrideKey+":", "removed:")
		f, ok := classifyWaveHealthDoc("probe.yaml", negativeWaveDoc(groupKind), stripped)
		if !ok || f.allowed {
			t.Errorf("%s was still allowed with %q removed from the overlay — the guard is not "+
				"actually checking that key, so it would not notice the override being deleted",
				groupKind, rule.overrideKey)
		}
	}
	if checked == 0 {
		t.Fatal("no override-backed kinds were checked — this control is vacuous")
	}
}

// negativeWaveDoc builds the minimal manifest the classifier grades: a resource
// of the given group/Kind at a negative sync wave, which is the only shape that
// reaches the override check.
func negativeWaveDoc(groupKind string) waveHealthDoc {
	group, kind, _ := strings.Cut(groupKind, "/")
	apiVersion := "v1"
	if group != "" {
		apiVersion = group + "/v1"
	}
	var d waveHealthDoc
	d.APIVersion = apiVersion
	d.Kind = kind
	// A name no AllowedNames entry claims, so the per-resource exception path
	// cannot wave this through and hide an unbacked kind.
	d.Metadata.Name = "coupling-probe"
	d.Metadata.Annotations = map[string]string{"argocd.argoproj.io/sync-wave": "-5"}
	return d
}

// AND THE SHIPPED FILE MUST BE THE RENDERED ONE. The guard reads the committed
// appvalues.yaml off disk, not the renderer, so a committed copy that has drifted
// from the code would satisfy the test above and still be what the guard reads.
// (render --check enforces this repo-wide; asserted here too because THIS guard's
// correctness depends on it and a reader of this file should not have to know
// that.)
func TestTheCommittedOverlayIsWhatTheGuardReads(t *testing.T) {
	p := filepath.Join("..", "..", "..", "..", "..",
		"instance-template", "apl-values", "_shared", "apl-overlay", OverrideSourceFile)
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read committed overlay: %v", err)
	}
	if string(got) != clusterspec.RenderAppValuesOverlayShared() {
		t.Errorf("%s has drifted from RenderAppValuesOverlayShared — re-run `llz render` and commit. "+
			"This guard reads the COMMITTED file, so drift here means it is vouching for the wrong content", p)
	}
}
