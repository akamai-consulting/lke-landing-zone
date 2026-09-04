package assertplatform

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/bootstrapcluster"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// baselineImage is an operator image on the version this llz release targets.
func baselineImage() string {
	return "docker.io/linode/apl-core:" + clusterspec.BaselineAplChartVersion
}

// deployJSON builds a `kubectl get deploy -o json` body with one Deployment
// carrying the given labels and a single apl-operator container on `image`.
func deployJSON(name string, labels map[string]string, image string) []byte {
	return deployJSONContainers(name, labels, map[string]string{containerName: image})
}

// deployJSONContainers is the same with an explicit container name→image map, for
// the sidecar and wrong-name arms.
func deployJSONContainers(name string, labels map[string]string, containers map[string]string) []byte {
	var b strings.Builder
	b.WriteString(`{"items":[{"metadata":{"name":"` + name + `","labels":{`)
	first := true
	for k, v := range labels {
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, "%q:%q", k, v)
	}
	b.WriteString(`}},"spec":{"template":{"spec":{"containers":[`)
	first = true
	for n, img := range containers {
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, `{"name":%q,"image":%q}`, n, img)
	}
	b.WriteString(`]}}}}]}`)
	return []byte(b.String())
}

// THE POSITIVE CONTROL, and it comes first deliberately: every arm below asserts a
// FAILURE, and a harness that cannot produce a pass would make all of them pass for
// the wrong reason. If this one breaks, none of the others mean anything.
func TestAplDeployedMatchingBaselinePasses(t *testing.T) {
	v := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel: aplOperatorName,
	}, baselineImage()), nil)
	if v.Err != nil {
		t.Fatalf("a cluster on the baseline must pass: %v", v.Err)
	}
	if v.Warn != "" {
		t.Errorf("a cluster on the baseline must not warn, got %q", v.Warn)
	}
	if v.Live != clusterspec.BaselineAplChartVersion {
		t.Errorf("live = %q, want the baseline %q", v.Live, clusterspec.BaselineAplChartVersion)
	}
}

// THE REGRESSION THIS LANE SHIPPED, reproduced from a REAL managed cluster.
//
// The first cut read `helm.sh/chart`, falling back to `app.kubernetes.io/version`.
// Both are written by the apl-operator SUB-chart's common-labels helper from that
// sub-chart's own Chart.yaml (version: 0.2.0, appVersion: "1.16.0") — constants of
// the operator's packaging that do not move when the PLATFORM moves. A cluster
// running v6.2.1 therefore read as "apl-core 0.2.0, a MAJOR apart", and release-e2e
// went red against a platform that was perfectly in step.
//
// The fixture carries the exact label values a real cluster showed, so the old
// reading cannot be restored without this going red. It is also why there is no
// fallback any more: BOTH labels are wrong, so either one as a second source
// reintroduces the bug.
func TestAplDeployedIgnoresTheSubChartsOwnLabels(t *testing.T) {
	raw := deployJSON("apl-operator", map[string]string{
		nameLabel:                   aplOperatorName,
		"helm.sh/chart":             "apl-operator-0.2.0",
		"app.kubernetes.io/version": "1.16.0",
	}, baselineImage())

	v := evaluateAplDeployed(raw, nil)
	if v.Err != nil {
		t.Fatalf("a cluster on the baseline must pass even though the sub-chart labels read 0.2.0/1.16.0: %v", v.Err)
	}
	if v.Live != clusterspec.BaselineAplChartVersion {
		t.Fatalf("live = %q, want %q — the sub-chart's packaging version was read as the platform version, "+
			"which is exactly the bug release-e2e caught", v.Live, clusterspec.BaselineAplChartVersion)
	}
	for _, forbidden := range []string{"0.2.0", "1.16.0"} {
		if strings.Contains(v.Live, forbidden) {
			t.Errorf("live = %q must never come from the sub-chart's own coordinates (%s)", v.Live, forbidden)
		}
	}
}

// FAIL CLOSED ON EVERY FORM OF "COULD NOT TELL". Each of these is a state in which
// the deployed version is unknown, and a lane that returned success from any of
// them would look exactly like a cluster in lockstep.
func TestAplDeployedFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		raw     []byte
		err     error
		wantMsg string
	}{
		{"unreachable cluster", nil, errors.New("connection refused"), "UNKNOWN"},
		{"unparseable listing", []byte("not json"), nil, "did not parse"},
		{"no deployments at all", []byte(`{"items":[]}`), nil, "no Deployment at all"},
		{
			"the operator Deployment is absent",
			deployJSON("something-else", map[string]string{nameLabel: "something-else"}, baselineImage()),
			nil, "Deployments present: something-else",
		},
		{
			"no container by that name",
			deployJSONContainers("apl-operator", map[string]string{nameLabel: aplOperatorName},
				map[string]string{"istio-proxy": "istio:1.0.0", "other": "other:1.0.0"}),
			nil, "no container named",
		},
		{
			"a digest-pinned image carries no tag",
			deployJSON("apl-operator", map[string]string{nameLabel: aplOperatorName},
				"linode/apl-core@sha256:"+strings.Repeat("a", 64)),
			nil, "carries no tag",
		},
		{
			"a tag that is not a version",
			deployJSON("apl-operator", map[string]string{nameLabel: aplOperatorName}, "linode/apl-core:main"),
			nil, "not a version this llz can compare",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := evaluateAplDeployed(tc.raw, tc.err)
			if v.Err == nil {
				t.Fatalf("%s must FAIL — an unreadable version is not a matching one", tc.name)
			}
			if !strings.Contains(v.Err.Error(), tc.wantMsg) {
				t.Errorf("error should contain %q, got: %v", tc.wantMsg, v.Err)
			}
		})
	}
}

// A NON-SEMVER TAG IS A REAL apl-core STATE AND MUST BE NAMED. `otomi.version`
// accepts a branch name, and the chart keys pullPolicy off exactly that distinction,
// so "main" means a floating dev install rather than a corrupt read. The failure has
// to print the tag: "we could not reach the cluster" and "the platform is on a
// branch build" have nothing in common as remedies.
func TestAplDeployedNamesANonSemverTag(t *testing.T) {
	v := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel: aplOperatorName,
	}, "linode/apl-core:main"), nil)
	if v.Err == nil {
		t.Fatal("a tag llz cannot grade is UNKNOWN, and unknown fails")
	}
	if !strings.Contains(v.Err.Error(), `"main"`) {
		t.Errorf("the failure must name the tag it found, got: %v", v.Err)
	}
	if v.Live != "main" {
		t.Errorf("the unusable tag must still be carried on the verdict, got %q", v.Live)
	}
}

// THE TAG PARSER, and every arm here is a reference form that really occurs.
func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"linode/apl-core:v6.2.1":                                   "v6.2.1",
		"docker.io/linode/apl-core:v6.2.1":                         "v6.2.1",
		"linode/apl-core:v6.3.0-rc.1":                              "v6.3.0-rc.1",
		"apl-core:v6.2.1":                                          "v6.2.1",
		"registry.example:5000/linode/apl-core:v6.2.1":             "v6.2.1",
		"registry.example:5000/linode/apl-core":                    "", // a port is NOT a tag
		"linode/apl-core":                                          "",
		"linode/apl-core@sha256:" + strings.Repeat("a", 64):        "", // digest only: no tag
		"linode/apl-core:v6.2.1@sha256:" + strings.Repeat("a", 64): "v6.2.1",
	}
	for image, want := range cases {
		if got := imageTag(image); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", image, got, want)
		}
	}
}

// A SIDECAR MUST NOT BE READ AS THE OPERATOR. A service mesh injects one without
// asking, and "the first container" is then a coin toss that reports istio's version
// as the platform's.
func TestAplDeployedPicksTheOperatorContainer(t *testing.T) {
	v := evaluateAplDeployed(deployJSONContainers("apl-operator",
		map[string]string{nameLabel: aplOperatorName},
		map[string]string{"istio-proxy": "istio/proxyv2:1.20.0", containerName: baselineImage()}), nil)
	if v.Err != nil {
		t.Fatalf("an injected sidecar must not break the read: %v", v.Err)
	}
	if v.Live != clusterspec.BaselineAplChartVersion {
		t.Errorf("live = %q, want %q — a sidecar's tag was read as the platform version",
			v.Live, clusterspec.BaselineAplChartVersion)
	}
}

// A SINGLE-CONTAINER DEPLOYMENT IS THE ONE RELAXATION, so an upstream rename of the
// container alone does not blind the lane while the answer is unambiguous.
func TestAplDeployedAcceptsASoleContainerUnderAnotherName(t *testing.T) {
	v := evaluateAplDeployed(deployJSONContainers("apl-operator",
		map[string]string{nameLabel: aplOperatorName},
		map[string]string{"operator": baselineImage()}), nil)
	if v.Err != nil {
		t.Fatalf("a sole container is unambiguous and must be read: %v", v.Err)
	}
	if v.Live != clusterspec.BaselineAplChartVersion {
		t.Errorf("live = %q, want %q", v.Live, clusterspec.BaselineAplChartVersion)
	}
}

// A RELEASE CANDIDATE IS AN ORDINARY PLATFORM VERSION. clusterspec.AplSemver
// tolerates the suffix on purpose, and an rc must reach a VERDICT rather than the
// unparseable failure arm.
func TestAplDeployedToleratesAPreReleaseTag(t *testing.T) {
	v := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel: aplOperatorName,
	}, "linode/apl-core:"+clusterspec.BaselineAplChartVersion+"-rc.1"), nil)
	if v.Err != nil {
		t.Errorf("an rc of the baseline must not fail the lane: %v", v.Err)
	}
}

// THE COUPLING THAT MAKES THIS ONE POLICY RATHER THAN TWO. The lane must reach the
// same verdict the SPEC-side gate reaches for the same version, so it asks
// clusterspec.AplChartDriftBlocks — the shared predicate BOTH gates decide on —
// rather than restating thresholds that would drift apart on the first bump.
//
// IT USED TO COMPARE AGAINST AplChartDriftOf, WHICH IS WHY IT MISSED A REAL BUG.
// The classifier answers "how far apart", not "does that block": the escape hatch
// lives in the second question, so a lane that consulted only the first failed on
// major drift with no way to override — and this test, asking the same incomplete
// question, agreed with it.
func TestAplDeployedPolicyMatchesTheSpecSidePolicy(t *testing.T) {
	for _, live := range []string{
		clusterspec.BaselineAplChartVersion, "6.2.0", "6.1.0", "6.0.0", "5.0.0", "7.0.0", "6.9.9",
	} {
		got := classifyAplDeployed(live)
		drift := clusterspec.AplChartDriftOf(live)
		switch {
		case drift == clusterspec.AplChartDriftNone:
			if got.Err != nil || got.Warn != "" {
				t.Errorf("%s is DriftNone for the spec gate, so the live lane must pass silently; got err=%v warn=%q", live, got.Err, got.Warn)
			}
		case clusterspec.AplChartDriftBlocks(drift):
			if got.Err == nil {
				t.Errorf("%s BLOCKS on the spec side — the live lane must fail too", live)
			}
		default:
			if got.Err != nil {
				t.Errorf("%s does not block on the spec side, so the live lane must not fail: %v", live, got.Err)
			}
			if got.Warn == "" {
				t.Errorf("%s is permitted drift and must WARN — silence is how a rollout lag stops being visible", live)
			}
		}
	}
}

// THE ESCAPE HATCH, and on managed App Platform it is the arm that matters most:
// LINODE moves the version, so a major roll is a condition the operator can
// neither fix nor revert. Without this the lane reddened a Gating:true check in
// both assert-suite and the scheduled health check, permanently, with no override
// — while the spec-side gate had honoured LLZ_ALLOW_APL_CHART_MAJOR_DRIFT all along.
func TestAplDeployedMajorDriftHonoursTheEscapeHatch(t *testing.T) {
	const major = "7.0.0"
	if got := classifyAplDeployed(major); got.Err == nil {
		t.Fatal("without the override a major apart must fail")
	} else if !strings.Contains(got.Err.Error(), clusterspec.AllowMajorDriftEnv) {
		t.Errorf("the failure must name the override, or the operator cannot find it: %v", got.Err)
	}

	t.Setenv(clusterspec.AllowMajorDriftEnv, "1")
	got := classifyAplDeployed(major)
	if got.Err != nil {
		t.Errorf("%s=1 must permit a staged major on the live lane exactly as it does on the spec side: %v",
			clusterspec.AllowMajorDriftEnv, got.Err)
	}
	if got.Warn == "" {
		t.Error("a staged major must still WARN — the override suppresses the block, not the visibility")
	}
}

// The warning has to be actionable on a managed cluster, where the reader cannot
// move the version themselves: it must name both versions and say who owns the roll.
func TestAplDeployedWarningNamesBothVersionsAndTheOwner(t *testing.T) {
	v := classifyAplDeployed("6.1.0")
	if v.Warn == "" {
		t.Fatal("a minor behind must warn")
	}
	for _, want := range []string{"6.1.0", clusterspec.BaselineAplChartVersion, "Linode owns the rollout"} {
		if !strings.Contains(v.Warn, want) {
			t.Errorf("warning must contain %q, got: %s", want, v.Warn)
		}
	}
}

// A SIBLING IN THE SAME NAMESPACE IS NOT APL-CORE. Reading "the first Deployment
// with a container" reports a neighbour's image tag as apl-core's — and hard-fails
// a gating lane on a cluster that is perfectly in step. The neighbour here sorts
// first and is a MAJOR away, so an unfiltered read fails loudly.
func TestAplDeployedIgnoresSiblingDeployments(t *testing.T) {
	raw := []byte(`{"items":[` +
		`{"metadata":{"name":"aaa-other","labels":{"app.kubernetes.io/name":"other"}},` +
		`"spec":{"template":{"spec":{"containers":[{"name":"other","image":"other:1.0.0"}]}}}},` +
		`{"metadata":{"name":"apl-operator","labels":{"app.kubernetes.io/name":"apl-operator"}},` +
		`"spec":{"template":{"spec":{"containers":[{"name":"apl-operator","image":"` + baselineImage() + `"}]}}}}` +
		`]}`)
	v := evaluateAplDeployed(raw, nil)
	if v.Err != nil {
		t.Fatalf("a neighbouring chart must not be read as apl-core: %v", v.Err)
	}
	if v.Live != clusterspec.BaselineAplChartVersion {
		t.Errorf("live = %q, want apl-core's own %q — a sibling's version was read instead", v.Live, clusterspec.BaselineAplChartVersion)
	}
}

// ...and when apl-core's operator is genuinely absent, a namespace full of other
// charts must still FAIL. Filtering must not become a way to skip.
func TestAplDeployedFailsWhenOnlySiblingsArePresent(t *testing.T) {
	v := evaluateAplDeployed(deployJSONContainers("other", map[string]string{nameLabel: "other"},
		map[string]string{"other": "other:1.0.0"}), nil)
	if v.Err == nil {
		t.Fatal("no apl-operator Deployment must FAIL — a filtered query that matches nothing is the vacuous pass this lane refuses")
	}
	if !strings.Contains(v.Err.Error(), "other") {
		t.Errorf("the failure must name what IS present, got: %v", v.Err)
	}
}

// THE DUPLICATION THIS PACKAGE IS FORCED TO CARRY, pinned so it cannot rot.
//
// bootstrapcluster exports the same two strings and annotates the same Deployment,
// but `internal/extensions` packages may not import each other — production code
// aliasing them fails TestNoNewExtensionToExtensionImports, and the sanctioned fix
// (splitting bootstrapcluster's library half into internal/shared) is a far wider
// change than two constants justify. A TEST may cross that boundary where the
// package may not, so the rename-one-side-only failure the rule would otherwise
// invite still cannot land quietly: rename either copy and this goes red.
func TestAplOperatorNamesAgreeWithBootstrapcluster(t *testing.T) {
	if aplOperatorNamespace != bootstrapcluster.AplOperatorNamespace {
		t.Errorf("namespace %q != bootstrapcluster's %q — the lane would read a namespace nothing installs into",
			aplOperatorNamespace, bootstrapcluster.AplOperatorNamespace)
	}
	if aplOperatorName != bootstrapcluster.AplOperatorDeployment {
		t.Errorf("deployment %q != bootstrapcluster's %q — the lane would select a workload that does not exist",
			aplOperatorName, bootstrapcluster.AplOperatorDeployment)
	}
}

// THE REMEDY MUST MATCH THE DIRECTION OF THE GAP. One blanket sentence got it wrong
// half the time: "upgrade llz to a release that targets this platform" is impossible
// when the CLUSTER is the old one, because no newer llz targets apl-core 5.x. An
// instruction that cannot be followed is worse than none — it spends the reader's
// time before they work out it is wrong.
func TestBlockingRemedyMatchesTheDriftDirection(t *testing.T) {
	behind := classifyAplDeployed("5.0.0")
	if behind.Err == nil {
		t.Fatal("a major behind must block")
	}
	if strings.Contains(behind.Err.Error(), "Upgrade llz to a release that targets this platform") {
		t.Error("that remedy is impossible for a cluster on an older major — no newer llz targets apl-core 5.x")
	}
	if !strings.Contains(behind.Err.Error(), "Linode owns the rollout") {
		t.Errorf("it must say who can actually move it, got: %v", behind.Err)
	}

	ahead := classifyAplDeployed("9.0.0")
	if ahead.Err == nil {
		t.Fatal("a major ahead must block")
	}
	if !strings.Contains(ahead.Err.Error(), "Upgrade llz") {
		t.Errorf("a cluster ahead of llz IS fixed by upgrading llz, got: %v", ahead.Err)
	}
	// Both directions must still offer the override, which is the only lever an
	// adopter holds on a platform Linode moves.
	for _, v := range []AplDeployedVerdict{behind, ahead} {
		if !strings.Contains(v.Err.Error(), clusterspec.AllowMajorDriftEnv) {
			t.Errorf("both directions must name the override, got: %v", v.Err)
		}
	}
}

// EVERY VERDICT MUST SAY WHERE IT LOOKED. This is what made the sub-chart-label bug
// diagnosable from a CI log alone: the failure printed the source it read, so the
// wrong source was visible without touching a cluster.
func TestVerdictNamesItsSource(t *testing.T) {
	v := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel: aplOperatorName,
	}, baselineImage()), nil)
	if v.Source != imageTagSource {
		t.Errorf("source = %q, want %q", v.Source, imageTagSource)
	}
	blocked := classifyAplDeployed("9.0.0")
	if !strings.Contains(blocked.Err.Error(), imageTagSource) {
		t.Errorf("a blocking failure must name where it read the version, got: %v", blocked.Err)
	}
}

// A STAGED MAJOR IS NOT A PATCH LAG. Both reach the permitted-drift arm — a
// minor/patch gap outright, and a MAJOR one once the override is set — and one
// sentence covered both, so a deliberately staged 7.0.0 against a v6.2.1 baseline
// read in the weekly check exactly like a point-release lag. The override suppresses
// the block, not the distance.
func TestStagedMajorReadsDifferentlyFromAPatchLag(t *testing.T) {
	t.Setenv(clusterspec.AllowMajorDriftEnv, "1")
	staged := classifyAplDeployed("7.0.0")
	if staged.Err != nil {
		t.Fatalf("the override must permit it: %v", staged.Err)
	}
	if !strings.Contains(staged.Warn, "MAJOR") || !strings.Contains(staged.Warn, clusterspec.AllowMajorDriftEnv) {
		t.Errorf("a staged major must say what it is and why it is not failing, got %q", staged.Warn)
	}
	if strings.Contains(staged.Warn, "routine mid-rollout") {
		t.Errorf("a major apart is not the routine mid-rollout state, got %q", staged.Warn)
	}

	lag := classifyAplDeployed("6.1.0")
	if !strings.Contains(lag.Warn, "routine mid-rollout") {
		t.Errorf("a minor lag IS the routine state, got %q", lag.Warn)
	}
	if strings.Contains(lag.Warn, clusterspec.AllowMajorDriftEnv) {
		t.Errorf("a patch lag has nothing to do with the override, got %q", lag.Warn)
	}
}
