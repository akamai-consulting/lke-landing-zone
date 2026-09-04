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
}

// THE TAG IS THE SOURCE, PINNED WITHOUT REFERENCE TO THE BASELINE.
//
// The regression test above derives its image tag from BaselineAplChartVersion, so
// it stays green for any baseline and cannot show WHICH field was read — it proves
// the labels are ignored, not that the tag is used. This one pins a literal tag
// that is deliberately NOT the baseline and requires it to come back verbatim, so
// "read the tag" is asserted independently of the constant it is graded against.
func TestLiveVersionComesFromTheTagNotTheBaseline(t *testing.T) {
	v := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel:                   aplOperatorName,
		"helm.sh/chart":             "apl-operator-0.2.0",
		"app.kubernetes.io/version": "1.16.0",
	}, "docker.io/linode/apl-core:v6.2.0"), nil)
	if v.Err != nil {
		t.Fatalf("a one-patch gap must not fail: %v", v.Err)
	}
	if v.Live != "v6.2.0" {
		t.Errorf("live = %q, want the literal tag \"v6.2.0\" — the version must come from the image tag", v.Live)
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
		{"unreachable cluster", nil, errors.New("connection refused"), "connection refused"},
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
			nil, "carries no usable tag",
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
	// A RAW LITERAL, not a map: the sidecar must be FIRST for this to be adversarial,
	// and Go map iteration order would make that incidental rather than pinned.
	raw := []byte(`{"items":[{"metadata":{"name":"apl-operator","labels":{"app.kubernetes.io/name":"apl-operator"}},` +
		`"spec":{"template":{"spec":{"containers":[` +
		`{"name":"istio-proxy","image":"istio/proxyv2:1.20.0"},` +
		`{"name":"apl-operator","image":"` + baselineImage() + `"}]}}}}]}`)
	v := evaluateAplDeployed(raw, nil)
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
		t.Fatalf("an rc of the baseline must not fail the lane: %v", v.Err)
	}
	if v.Warn != "" {
		t.Errorf("an rc of the baseline is not drift and must be silent, got %q", v.Warn)
	}
	if v.Live != clusterspec.BaselineAplChartVersion+"-rc.1" {
		t.Errorf("live = %q, want the rc tag verbatim", v.Live)
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

// THE DUPLICATION THIS PACKAGE IS FORCED TO CARRY (see the const block), pinned so
// it cannot rot. A TEST may cross the extension-to-extension boundary that
// production code may not, so renaming either copy goes red here.
func TestAplOperatorNamesAgreeWithBootstrapcluster(t *testing.T) {
	if aplOperatorNamespace != bootstrapcluster.AplOperatorNamespace {
		t.Errorf("namespace %q != bootstrapcluster's %q — the lane would read a namespace nothing installs into",
			aplOperatorNamespace, bootstrapcluster.AplOperatorNamespace)
	}
	if aplOperatorName != bootstrapcluster.AplOperatorDeployment {
		t.Errorf("deployment %q != bootstrapcluster's %q — the lane would select a workload that does not exist",
			aplOperatorName, bootstrapcluster.AplOperatorDeployment)
	}
	// containerName is a THIRD constant so an upstream rename of the container can
	// move independently — but it derives from `{{ .Chart.Name }}`, which is the same
	// string today. Pin the current equality so a one-sided edit is visible.
	if containerName != aplOperatorName {
		t.Errorf("container %q != %q; apl-core names the container {{ .Chart.Name }}. If upstream really renamed it, "+
			"update this expectation deliberately", containerName, aplOperatorName)
	}
}

// The remedy must match the direction of the gap — see classifyAplDeployed. This
// pins it: an instruction that cannot be followed is worse than none.
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

// A staged major is not a patch lag — see classifyAplDeployed. The override
// suppresses the block, not the distance.
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

// A FOREIGN SOLE CONTAINER MUST BE REFUSED. The relaxation exists so an upstream
// rename of the CONTAINER does not blind the lane — not so any lone workload gets
// its tag read as the platform version. Ungated it reported istio's 1.20.0 as
// "apl-core 1.20.0, a MAJOR apart, raise it with Linode", and a sole container
// tagged v6.2.1 would have PASSED on a version never read from apl-core.
//
// The safe half is covered by TestAplDeployedAcceptsASoleContainerUnderAnotherName;
// this is the half that was missing, and it is where the bug would live.
func TestSoleContainerRelaxationRefusesAForeignImage(t *testing.T) {
	for _, image := range []string{
		"quay.io/someone/totally-different:1.2.3",
		"istio/proxyv2:1.20.0",
		// The dangerous one: a foreign image whose tag would otherwise pass silently.
		"quay.io/someone/totally-different:" + clusterspec.BaselineAplChartVersion,
	} {
		v := evaluateAplDeployed(deployJSONContainers("apl-operator",
			map[string]string{nameLabel: aplOperatorName},
			map[string]string{"operator": image}), nil)
		if v.Err == nil {
			t.Errorf("a sole %q container is not apl-core and must not be read as it (live=%q)", image, v.Live)
		}
	}
}

// AN UNSET otomi.version COMES BACK AS THE SUB-CHART'S appVersion (1.16.0), via
// Helm's `| default .Chart.AppVersion` — see implausibleMajor. Graded as drift that
// is a fleet-wide red about a gap that does not exist.
func TestATagPredatingEveryBaselineIsNotAPlatformVersion(t *testing.T) {
	for _, tag := range []string{"1.16.0", "0.2.0", "1.20.0"} {
		v := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
			nameLabel: aplOperatorName,
		}, "docker.io/linode/apl-core:"+tag), nil)
		if v.Err == nil {
			t.Fatalf("tag %q predates every apl-core release and must not be graded as drift (live=%q, warn=%q)",
				tag, v.Live, v.Warn)
		}
		if strings.Contains(v.Err.Error(), "raise it with them") {
			t.Errorf("tag %q must not be reported as an old PLATFORM — that remedy sends the operator to Linode "+
				"about a version gap that does not exist: %v", tag, v.Err)
		}
		if !strings.Contains(v.Err.Error(), "otomi.version") {
			t.Errorf("the failure must name the likely cause so the reader can check it, got: %v", v.Err)
		}
	}
	// ...and the guard must not swallow a genuine old MAJOR that llz really did target.
	old := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel: aplOperatorName,
	}, "docker.io/linode/apl-core:6.0.0"), nil)
	if old.Err != nil && strings.Contains(old.Err.Error(), "predates every apl-core release") {
		t.Errorf("6.0.0 IS a version llz targeted and must be graded as drift, not disqualified: %v", old.Err)
	}
}

// ZERO CONTAINERS IS A READ FAILURE, NOT A RENAME. Kubernetes cannot serve a
// Deployment with no containers, so an empty slice means the JSON shape this lane
// parses stopped matching kubectl's output. Reported as "no container named
// apl-operator … Containers present:" it sent the reader hunting for a renamed
// container behind a dangling empty list.
func TestZeroContainersIsReportedAsAReadFailure(t *testing.T) {
	raw := []byte(`{"items":[{"metadata":{"name":"apl-operator","labels":{"app.kubernetes.io/name":"apl-operator"}},` +
		`"spec":{"template":{"spec":{"containers":[]}}}}]}`)
	v := evaluateAplDeployed(raw, nil)
	if v.Err == nil {
		t.Fatal("no containers means the version is UNKNOWN, and unknown fails")
	}
	if !strings.Contains(v.Err.Error(), "no containers at all") {
		t.Errorf("it must say the listing carried no containers, not that one was renamed: %v", v.Err)
	}
	if strings.Contains(v.Err.Error(), "Containers present: .") {
		t.Errorf("a dangling empty list names nothing: %v", v.Err)
	}
}

// A NAMED CONTAINER WITH AN EMPTY IMAGE IS NOT A MISSING CONTAINER — collapsing
// them produces a message that denies the thing it then prints.
func TestNamedContainerWithNoImageIsItsOwnFailure(t *testing.T) {
	v := evaluateAplDeployed(deployJSONContainers("apl-operator",
		map[string]string{nameLabel: aplOperatorName},
		map[string]string{containerName: ""}), nil)
	if v.Err == nil {
		t.Fatal("an empty image means the version is UNKNOWN")
	}
	if strings.Contains(v.Err.Error(), "no container named") {
		t.Errorf("the container IS present — the message must not deny it: %v", v.Err)
	}
	if !strings.Contains(v.Err.Error(), "carries no image") {
		t.Errorf("it must name the actual state, got: %v", v.Err)
	}
}

// A CANDIDATE THAT CANNOT ANSWER MUST NOT END THE SCAN. The selector matches by
// name OR label, so a stale Deployment literally named `apl-operator` can sit
// beside the renamed real one carrying the label. Returning the first candidate's
// failure reported "unreadable" while the answer sat in the next item.
func TestAnUnreadableCandidateDoesNotHideALaterAnswer(t *testing.T) {
	raw := []byte(`{"items":[` +
		`{"metadata":{"name":"apl-operator","labels":{}},` +
		`"spec":{"template":{"spec":{"containers":[{"name":"legacy","image":"legacy:1.0.0"}]}}}},` +
		`{"metadata":{"name":"apl-operator-controller","labels":{"app.kubernetes.io/name":"apl-operator"}},` +
		`"spec":{"template":{"spec":{"containers":[{"name":"apl-operator","image":"` + baselineImage() + `"}]}}}}` +
		`]}`)
	v := evaluateAplDeployed(raw, nil)
	if v.Err != nil {
		t.Fatalf("a later candidate answers the question and must be reached: %v", v.Err)
	}
	if v.Live != clusterspec.BaselineAplChartVersion {
		t.Errorf("live = %q, want %q", v.Live, clusterspec.BaselineAplChartVersion)
	}
}

// ...and when NO candidate can answer, the held failure is reported rather than a
// vacuous "not found". Continuing the scan must not become a way to lose the reason.
func TestAllCandidatesUnreadableReportsTheFailure(t *testing.T) {
	v := evaluateAplDeployed(deployJSONContainers("apl-operator",
		map[string]string{nameLabel: aplOperatorName},
		map[string]string{"legacy": "legacy:1.0.0"}), nil)
	if v.Err == nil {
		t.Fatal("no candidate could answer — that is UNKNOWN, and unknown fails")
	}
	if !strings.Contains(v.Err.Error(), "Containers present") {
		t.Errorf("the held failure must survive, naming what was found: %v", v.Err)
	}
}

// EVERY UNREADABLE ARM CARRIES THE REMEDY — see unreadableRemedy. A fleet-wide gate
// that names no way out is a gate that gets switched off.
func TestUnreadableFailuresCarryTheRemedy(t *testing.T) {
	cases := map[string][]byte{
		"no deployment": deployJSONContainers("other", map[string]string{nameLabel: "other"},
			map[string]string{"other": "other:1.0.0"}),
		"no containers": []byte(`{"items":[{"metadata":{"name":"apl-operator","labels":{"app.kubernetes.io/name":"apl-operator"}},` +
			`"spec":{"template":{"spec":{"containers":[]}}}}]}`),
		"no such container": deployJSONContainers("apl-operator", map[string]string{nameLabel: aplOperatorName},
			map[string]string{"legacy": "legacy:1.0.0"}),
		"digest only": deployJSON("apl-operator", map[string]string{nameLabel: aplOperatorName},
			"linode/apl-core@sha256:"+strings.Repeat("a", 64)),
		"non-semver tag": deployJSON("apl-operator", map[string]string{nameLabel: aplOperatorName},
			"linode/apl-core:main"),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			v := evaluateAplDeployed(raw, nil)
			if v.Err == nil {
				t.Fatal("must fail")
			}
			if !strings.Contains(v.Err.Error(), "llz self-update") {
				t.Errorf("the remedy must be in the message: %v", v.Err)
			}
		})
	}
}

// THE UNTAGGED ARM MUST NOT ASSERT A CAUSE IT DID NOT MEASURE. imageTag collapses
// digest-pinned, empty-tag and no-tag into "", and they point at different
// subsystems — telling a private-registry operator their image is digest-pinned
// sends them to the wrong one.
func TestUntaggedImageNamesTheActualReason(t *testing.T) {
	digest := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{nameLabel: aplOperatorName},
		"linode/apl-core@sha256:"+strings.Repeat("a", 64)), nil)
	if digest.Err == nil || !strings.Contains(digest.Err.Error(), "digest") {
		t.Errorf("a digest pin must be named as one: %v", digest.Err)
	}
	empty := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{nameLabel: aplOperatorName},
		"registry.example:5000/linode/apl-core"), nil)
	if empty.Err == nil {
		t.Fatal("an untagged reference is UNKNOWN")
	}
	// ASSERT ON THE REASON CLAUSE, NOT THE WHOLE MESSAGE. The shared remedy
	// paragraph contains the word "digest-locked", so a Contains(err, "digest") over
	// the full report is satisfied by text that has nothing to do with the finding.
	if strings.Contains(empty.Err.Error(), "pinned by digest") {
		t.Errorf("this reference is not digest-pinned — saying so sends the reader to the wrong subsystem: %v", empty.Err)
	}
	if !strings.Contains(empty.Err.Error(), "names no tag at all") {
		t.Errorf("it must name the actual reason, got: %v", empty.Err)
	}
}

// THE CLASSIFIER'S SAFETY IS LOCAL. AplChartDriftOf answers DriftNone for "" and
// Unparseable for a malformed version, and both used to fall through to a silent
// pass or a "routine mid-rollout" warning — a vacuous green from the function whose
// job is to refuse one. evaluateAplDeployed cannot reach either today; nothing
// stops a future caller.
//
// Note the shape this guards against: TestAplDeployedPolicyMatchesTheSpecSidePolicy
// derives its expectation from the same predicates the code calls, so adding an
// unparseable version to ITS table would have made the test REQUIRE the fail-open.
func TestClassifierRefusesWhatItCannotGrade(t *testing.T) {
	for _, live := range []string{"", "garbage", "not-a-version"} {
		got := classifyAplDeployed(live)
		if got.Err == nil {
			t.Errorf("classifyAplDeployed(%q) must FAIL, got err=nil warn=%q — a version it cannot grade is UNKNOWN",
				live, got.Warn)
		}
	}
}

// THE LANE QUERIES WHAT IT CLAIMS TO. The namespace constant is pinned against
// bootstrapcluster's copy, but nothing asserted it reaches the actual command — a
// dropped `-o json` would have been caught by no unit test.
func TestLaneQueriesTheOperatorDeployment(t *testing.T) {
	orig := deps
	t.Cleanup(func() { deps = orig })

	var gotName string
	var gotArgs []string
	Install(Deps{Exec: func(name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return deployJSON("apl-operator", map[string]string{nameLabel: aplOperatorName}, baselineImage()), nil
	}})
	if err := assertAplDeployedVersion(); err != nil {
		t.Fatalf("a cluster on the baseline must pass the lane: %v", err)
	}
	if gotName != "kubectl" {
		t.Errorf("command = %q, want kubectl", gotName)
	}
	want := []string{"-n", aplOperatorNamespace, "get", "deploy", "-o", "json"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", gotArgs, want)
	}
}

// A FAILING VERDICT MUST REACH THE CALLER AS AN ERROR, and a warning must NOT.
// The lane's exit status is what the suite gates on.
func TestLanePropagatesTheVerdict(t *testing.T) {
	orig := deps
	t.Cleanup(func() { deps = orig })

	Install(Deps{Exec: func(string, ...string) ([]byte, error) {
		return deployJSON("apl-operator", map[string]string{nameLabel: aplOperatorName},
			"docker.io/linode/apl-core:9.0.0"), nil
	}})
	if err := assertAplDeployedVersion(); err == nil {
		t.Error("a major apart must fail the lane")
	}

	Install(Deps{Exec: func(string, ...string) ([]byte, error) {
		return deployJSON("apl-operator", map[string]string{nameLabel: aplOperatorName},
			"docker.io/linode/apl-core:6.1.0"), nil
	}})
	if err := assertAplDeployedVersion(); err != nil {
		t.Errorf("a minor lag warns and must NOT fail: %v", err)
	}

	// A PARTIALLY-POPULATED Deps MUST FAIL CLOSED, NOT PANIC. Install replaces the
	// whole struct, so a literal that omits Exec nils out a field the package
	// documents as "defaulting to implementations that work rather than to nil
	// funcs" — and the lane called it, segfaulting instead of reporting UNKNOWN.
	Install(Deps{})
	if err := assertAplDeployedVersion(); err == nil {
		t.Error("an un-installed Exec seam reads nothing, and nothing is UNKNOWN")
	}
}

// A FOREIGN IMAGE ON THE PRIMARY PATH MUST BE REFUSED, not just on the relaxation
// — see operatorImage. A foreign image tagged with the baseline is a wrong GREEN on
// a gating lane, which is the one failure this lane must never produce.
func TestForeignImageIsRefusedOnTheNamedContainerPath(t *testing.T) {
	for _, image := range []string{
		"docker.io/evilcorp/backdoor:" + clusterspec.BaselineAplChartVersion,
		"quay.io/someone/totally-different:v6.2.1",
	} {
		raw := []byte(`{"items":[{"metadata":{"name":"apl-operator","labels":{}},` +
			`"spec":{"template":{"spec":{"containers":[{"name":"apl-operator","image":"` + image + `"}]}}}}]}`)
		v := evaluateAplDeployed(raw, nil)
		if v.Err == nil {
			t.Errorf("%q is not apl-core and must not be read as the platform version (live=%q)", image, v.Live)
		}
	}
	// The real image on the same path still passes — the gate must not break the
	// case it exists to protect.
	ok := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{nameLabel: aplOperatorName},
		baselineImage()), nil)
	if ok.Err != nil {
		t.Errorf("apl-core's own image must still be read: %v", ok.Err)
	}
}

// A LABEL MATCH OUTRANKS A NAME-ONLY MATCH — see evaluateAplDeployed. Both
// orderings must give the same answer; that equality is the real assertion, because
// a lane whose result depends on list order is wrong in one of the two orders
// whatever the expected value is.
func TestAStaleNameMatchDoesNotBeatTheLabelledOperator(t *testing.T) {
	stale := `{"metadata":{"name":"apl-operator","labels":{}},"spec":{"template":{"spec":{"containers":[` +
		`{"name":"apl-operator","image":"docker.io/linode/apl-core:v5.0.0"}]}}}}`
	real := `{"metadata":{"name":"z-renamed-operator","labels":{"app.kubernetes.io/name":"apl-operator"}},` +
		`"spec":{"template":{"spec":{"containers":[{"name":"apl-operator","image":"` + baselineImage() + `"}]}}}}`

	for _, tc := range []struct{ name, body string }{
		{"stale first", stale + "," + real},
		{"stale last", real + "," + stale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := evaluateAplDeployed([]byte(`{"items":[`+tc.body+`]}`), nil)
			if v.Err != nil {
				t.Fatalf("the labelled operator answers the question and must win: %v", v.Err)
			}
			if v.Live != clusterspec.BaselineAplChartVersion {
				t.Errorf("live = %q, want %q — a stale name-match was read instead", v.Live, clusterspec.BaselineAplChartVersion)
			}
		})
	}
}

// AN UNINSTALLED SEAM IS A WIRING FAULT, NOT A PLATFORM CHANGE. Returning (nil, nil)
// fails closed, but reports "the listing did not parse as JSON" — sending the reader
// after a platform that changed shape when nobody called Install.
func TestUninstalledExecNamesTheWiringFault(t *testing.T) {
	orig := deps
	t.Cleanup(func() { deps = orig })

	Install(Deps{})
	err := assertAplDeployedVersion()
	if err == nil {
		t.Fatal("an un-installed seam reads nothing, and nothing is UNKNOWN")
	}
	if strings.Contains(err.Error(), "did not parse as JSON") {
		t.Errorf("that blames the platform for a wiring fault: %v", err)
	}
	if !strings.Contains(err.Error(), "never installed") {
		t.Errorf("the failure must name the actual fault, got: %v", err)
	}
}
