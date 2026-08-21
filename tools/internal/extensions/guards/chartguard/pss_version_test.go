package chartguard

// pss_version_test.go — THE GATE FOR ISSUE #447.
//
// `llz-cluster-foundation` labels its `restricted` namespaces with
// `pod-security.kubernetes.io/{enforce,warn,audit}-version`. That value was a
// hardcoded Kubernetes minor whose own comment said it "must match the cluster's
// Kubernetes minor" — a promise nothing kept and nothing checked. It sat at v1.33
// while the scaffold default moved to a v1.34 build, and would have kept sitting
// there.
//
// THE STALENESS WAS THE SYMPTOM, NOT THE DEFECT. This chart is published once and
// installed on every instance's cluster; LKE-Enterprise version availability is
// per-ACCOUNT and rotates within hours, so those clusters run different minors.
// "Matches the cluster's minor" is not a property one baked literal can have —
// whatever it names is wrong for somebody, and wrong SILENTLY: Pod Security
// Standards revisions are cumulative, so a lagging pin enforces a weaker ruleset
// than the `restricted` label advertises while looking identical in the labels.
//
// So the gate is not "is the pin current" — that question has no answer here. It
// is "does this chart pin a minor at all", which is statically decidable and is
// the thing that must not come back.
//
// WHY A TEST AND NOT A make lint GATE. It is one invariant on one file with no
// deps, and `make lint` runs `go test` on the module anyway. A registered `llz ci`
// verb costs a registry entry, a lint-group membership and a census line; that is
// the right price for a gate with a corpus, and the wrong one for this.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pssVersionLine matches the values.yaml assignment, quoted or not.
var pssVersionLine = regexp.MustCompile(`(?m)^podSecurityStandardsVersion:\s*"?([^"\s#]+)"?`)

// aKubernetesMinor is what must never appear here: v1.33, 1.33, v1.33.6+lke7 —
// any literal naming a specific Kubernetes release.
var aKubernetesMinor = regexp.MustCompile(`^v?\d+\.\d+`)

func TestTheChartDoesNotPinAKubernetesMinorItCannotKnow(t *testing.T) {
	root := repoRootForChartTest(t)
	path := filepath.Join(root, "kubernetes-charts", "llz-cluster-foundation", "values.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	m := pssVersionLine.FindSubmatch(raw)
	// FAIL CLOSED ON VACUITY. A renamed or deleted key must not read as "no minor
	// pinned, all good" — that is a gate passing having examined nothing, which is
	// indistinguishable from the drift it exists to catch.
	if m == nil {
		t.Fatalf("no podSecurityStandardsVersion assignment found in %s — if the key moved, move this "+
			"gate with it; if the PSS labels were dropped entirely, delete this test deliberately "+
			"rather than letting it pass on an empty match", path)
	}
	got := string(m[1])

	if aKubernetesMinor.MatchString(got) {
		t.Errorf("podSecurityStandardsVersion = %q — a published chart cannot name the Kubernetes "+
			"minor of every cluster it lands on, and a pin that lags enforces a WEAKER ruleset than "+
			"the `restricted` label advertises, silently. Use \"latest\": it is the upstream default "+
			"and is resolved by the apiserver that does the enforcing. If a pin is genuinely needed, "+
			"it has to come from the instance's own spec rather than from this file.", got)
	}
	if got != "latest" {
		t.Errorf("podSecurityStandardsVersion = %q, want \"latest\" — the only value correct on every "+
			"cluster this chart is installed on", got)
	}
}

// TestTheRestrictedNamespacesStillCarryAVersionLabel — the other direction. The
// fix for a stale pin must not be to drop the labels: an absent -version label
// also resolves to latest, but it does so by ACCIDENT, and it takes the value out
// of the one place a reader would look to find out what is enforced.
func TestTheRestrictedNamespacesStillCarryAVersionLabel(t *testing.T) {
	root := repoRootForChartTest(t)
	path := filepath.Join(root, "kubernetes-charts", "llz-cluster-foundation", "templates", "namespaces.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	tpl := string(raw)
	for _, mode := range []string{"enforce", "warn", "audit"} {
		label := "pod-security.kubernetes.io/" + mode + "-version"
		if !strings.Contains(tpl, label) {
			t.Errorf("the template no longer emits %s — the enforced ruleset must stay visible in "+
				"the namespace's own labels, not be inherited by omission", label)
		}
	}
}

func repoRootForChartTest(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "kubernetes-charts")); err != nil {
		t.Skipf("repo root not found at %s: %v", root, err)
	}
	return root
}
