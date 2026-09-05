package clusterspec

import (
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// ON BY DEFAULT, AND OFF ONLY WHEN SAID SO. The default moved, so the property
// worth pinning inverted with it: an unstated field must render (llz tracks the
// release baseline like every other version it pins), and ONLY an explicit
// `manageAplVersion: false` leaves the version with Linode.
//
// BOTH HALVES, because a tri-state read as a plain bool would still pass the first
// half by accident — nil and false both being "not true" is exactly the confusion
// the pointer exists to prevent.
func TestOtomiOverlayIsOnUnlessTurnedOff(t *testing.T) {
	if got := RenderOtomiOverlayEnv(Bootstrap{AplChartVersion: "v6.2.1"}); got == "" {
		t.Error("an unstated manageAplVersion must render otomi.yaml — the default is now ON")
	}
	if got := RenderOtomiOverlayEnv(Bootstrap{
		ManageAplVersion: boolPtr(false), AplChartVersion: "v6.2.1",
	}); got != "" {
		t.Errorf("an explicit manageAplVersion: false must render nothing, got:\n%s", got)
	}
}

// THE ONLY KEY WRITTEN IS THE ONE LLZ OWNS. spec.git lives in this same CR and is
// apl-core's BYO-Git wiring; the overlay reconciler deep-merges, so a fragment that
// emitted `git: {}` would take the platform's own git config out from under it.
func TestOtomiOverlayWritesOnlyTheVersion(t *testing.T) {
	out := RenderOtomiOverlayEnv(Bootstrap{ManageAplVersion: boolPtr(true), AplChartVersion: "v6.2.0"})
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
	pinned := RenderOtomiOverlayEnv(Bootstrap{ManageAplVersion: boolPtr(true), AplChartVersion: "6.1.0"})
	if !strings.Contains(pinned, "version: v6.1.0") {
		t.Errorf("an explicit pin is what deploys, in apl-core's spelling:\n%s", pinned)
	}
	// A letter-leading name is already what the pattern permits and is not ours to
	// reinterpret.
	named := RenderOtomiOverlayEnv(Bootstrap{ManageAplVersion: boolPtr(true), AplChartVersion: "main"})
	if !strings.Contains(named, "version: main") {
		t.Errorf("a branch-style version passes through untouched:\n%s", named)
	}
	unpinned := RenderOtomiOverlayEnv(Bootstrap{ManageAplVersion: boolPtr(true)})
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
		out := RenderOtomiOverlayEnv(Bootstrap{ManageAplVersion: boolPtr(true), AplChartVersion: pin})
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

// THE LIVE FILE, verbatim from a managed e2e cluster's apl-<env> branch
// (env/settings/otomi.yaml, written by apl-core itself — its commit subject is
// "updated values [ci skip]"). Every field beside `version` belongs to apl-core.
const liveOtomiCR = `kind: AplCapabilitySet
metadata:
    name: otomi
spec:
    aiEnabled: false
    hasExternalDNS: false
    hasExternalIDP: false
    isMultitenant: true
    isPreInstalled: true
    nodeSelector: {}
    useORCS: true
    version: v6.2.1
`

// LLZ OWNS ONE KEY OF A FILE APL-CORE CO-WRITES. A file-level write would blank
// seven other settings — isMultitenant and useORCS among them — which is why this
// path merges by key. The fixture is the real file, so a regression to a wholesale
// write cannot pass.
func TestSetOtomiVersionPreservesAplCoresOwnFields(t *testing.T) {
	updated, changed, err := SetOtomiVersion([]byte(liveOtomiCR), "v6.2.1-rc.2")
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if !changed {
		t.Fatal("a different version must register as a change")
	}
	got := string(updated)
	for _, keep := range []string{
		"aiEnabled", "hasExternalDNS", "hasExternalIDP",
		"isMultitenant", "isPreInstalled", "nodeSelector", "useORCS",
		"AplCapabilitySet", "otomi",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("merge dropped %q — apl-core owns that field:\n%s", keep, got)
		}
	}
	if !strings.Contains(got, "v6.2.1-rc.2") {
		t.Errorf("the asserted version is missing:\n%s", got)
	}
	// The values that were TRUE must still be true — presence of the key is not
	// enough, a merge that reset them to zero would still contain the name.
	for _, pair := range []string{"isMultitenant: true", "useORCS: true", "isPreInstalled: true"} {
		if !strings.Contains(got, pair) {
			t.Errorf("merge changed %q:\n%s", pair, got)
		}
	}
}

// AN UNCHANGED VERSION MUST NOT PUSH. apl-core rewrites this file on its own
// schedule; a reconciler that re-marshalled it every pass would churn a commit
// against it forever.
func TestSetOtomiVersionIsANoOpWhenAlreadyCorrect(t *testing.T) {
	updated, changed, err := SetOtomiVersion([]byte(liveOtomiCR), "v6.2.1")
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if changed {
		t.Error("the version already matches — that must not be a push")
	}
	if string(updated) != liveOtomiCR {
		t.Error("a no-op must return the bytes untouched, not a re-marshal")
	}
	// An empty desired version is "no opinion", not "blank it".
	if _, changed, _ := SetOtomiVersion([]byte(liveOtomiCR), "  "); changed {
		t.Error("an empty desired version must not rewrite the file")
	}
}

// The reader must find the version the renderer writes — the two halves of this
// channel, checked against each other rather than each against its own copy.
func TestOtomiOverlayVersionReadsWhatRenderWrote(t *testing.T) {
	b := Bootstrap{ManageAplVersion: boolPtr(true), AplChartVersion: "6.2.0"}
	src := RenderOtomiOverlayEnv(b)
	if src == "" {
		t.Fatal("premise: an opted-in bootstrap must render an overlay")
	}
	got, err := OtomiOverlayVersion([]byte(src))
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if got != "v6.2.0" {
		t.Errorf("version = %q, want %q", got, "v6.2.0")
	}
	// And an instance that has explicitly handed the version back renders nothing,
	// which reads as no opinion. Stated, not omitted: omission is now the opposite.
	if s := RenderOtomiOverlayEnv(Bootstrap{
		ManageAplVersion: boolPtr(false), AplChartVersion: "6.2.0",
	}); s != "" {
		t.Errorf("manageAplVersion: false — nothing must be rendered, got %q", s)
	}
}
