package assertplatform

// overlay_fixture_namespaces_test.go couples the namespaces the fixtures need to
// the namespaces the lane they run in actually creates.
//
// THE EMITTER DELIBERATELY CREATES NONE. Emitting a Namespace carrying only
// metadata.name and applying it three-way-merged every other field off the real
// one — verified live: `monitoring` lost `monitoring: enabled` and its -20
// sync-wave, in the same job as the admission gates that run after it. So the
// namespaces are the caller's, and FixtureNamespaces() was exported to say which
// ones the caller owes.
//
// IT WAS EXPORTED AND THEN NEVER CALLED. Nothing compared it to the workflow, so
// the coupling it exists to express was carried entirely by a comment. The failure
// that leaves is loud but expensive and misattributed: a new overlay row in an
// apl-core-owned namespace fails six minutes into CI, in a step called "Apply
// pre-overlay fixtures", with `namespaces "keycloak" not found` — a red about the
// apply, on a PR about a field map. This says it in a second, and names the cause.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestEveryFixtureNamespaceIsOneTheLaneActuallyCreates(t *testing.T) {
	repo := filepath.Join("..", "..", "..", "..", "..")

	// The kind lane gets its namespaces from two places, and both have to be read
	// or this test would report a false miss for whichever it skipped.
	created := map[string]bool{}

	// 1. The rendered Namespace objects — `yq 'select(.kind == "Namespace")'
	//    rendered/*.yaml`, which is llz-cluster-foundation's own list.
	fnd := filepath.Join(repo, "kubernetes-charts", "llz-cluster-foundation", "values.yaml")
	raw, err := os.ReadFile(fnd)
	if err != nil {
		t.Fatalf("read %s: %v", fnd, err)
	}
	var vals struct {
		Namespaces []struct {
			Name string `json:"name"`
		} `json:"namespaces"`
	}
	if err := yaml.Unmarshal(raw, &vals); err != nil {
		t.Fatalf("parse %s: %v", fnd, err)
	}
	for _, ns := range vals.Namespaces {
		created[ns.Name] = true
	}

	// 2. The stub loop for the apl-core-owned ones, read out of the workflow rather
	//    than transcribed — a transcription is the drift this test exists to catch.
	wf := filepath.Join(repo, ".github", "workflows", "lint.yml")
	wraw, err := os.ReadFile(wf)
	if err != nil {
		t.Fatalf("read %s: %v", wf, err)
	}
	stub := regexp.MustCompile(`for ns in ([a-z0-9 -]+); do`)
	m := stub.FindSubmatch(wraw)
	if m == nil {
		t.Fatalf("%s no longer stubs the apl-core-owned namespaces with a `for ns in …` loop — this "+
			"test reads that loop to know what the lane creates, so it has to be taught the new shape "+
			"rather than left matching nothing", wf)
	}
	for _, ns := range strings.Fields(string(m[1])) {
		created[ns] = true
	}
	// kind creates this one itself.
	created["kube-system"] = true
	created["default"] = true

	need := FixtureNamespaces()
	if len(need) == 0 {
		t.Fatal("FixtureNamespaces() names nothing — either the field map is empty or this test is " +
			"checking a list that no longer describes what the fixtures need")
	}
	for _, ns := range need {
		if !created[ns] {
			t.Errorf("the appliability fixtures land in namespace %q, which the kind lane never creates: "+
				"it is neither in llz-cluster-foundation's `namespaces:` list nor in lint.yml's stub loop. "+
				"`kubectl apply` of the fixtures would fail with `namespaces %q not found`, six minutes "+
				"into CI, in a step named for the apply rather than for the row that needs it. Add it to "+
				"one of those two places", ns, ns)
		}
	}
}
