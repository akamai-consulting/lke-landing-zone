package assertplatform

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/bootstrapcluster"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// deployJSON builds a `kubectl get deploy -o json` body with one Deployment
// carrying the given labels.
func deployJSON(name string, labels map[string]string) []byte {
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
	b.WriteString(`}}}]}`)
	return []byte(b.String())
}

// THE POSITIVE CONTROL, and it comes first deliberately: every arm below asserts a
// FAILURE, and a harness that cannot produce a pass would make all of them pass for
// the wrong reason. If this one breaks, none of the others mean anything.
func TestAplDeployedMatchingBaselinePasses(t *testing.T) {
	v := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		chartLabel: "apl-" + clusterspec.BaselineAplChartVersion,
	}), nil)
	if v.Err != nil {
		t.Fatalf("a cluster on the baseline must pass: %v", v.Err)
	}
	if v.Warn != "" {
		t.Errorf("a cluster on the baseline must not warn, got %q", v.Warn)
	}
	if v.Live != clusterspec.BaselineAplChartVersion {
		t.Errorf("live = %q, want the baseline %q", v.Live, clusterspec.BaselineAplChartVersion)
	}
	if v.Source != chartLabel {
		t.Errorf("source = %q, want the chart label — the image tag is a proxy and must never be the source", v.Source)
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
			"deployments with no version label",
			deployJSON("apl-operator", map[string]string{"app.kubernetes.io/name": "apl-operator"}),
			nil, "Deployments present: apl-operator",
		},
		{
			"a version label that does not parse",
			deployJSON("apl-operator", map[string]string{chartLabel: "apl-not-a-version"}),
			nil, "not a chart version",
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

// NAME WHAT IS PRESENT. If the label were renamed upstream, the absent name tells
// the reader nothing — the Deployments that DO exist are the only lead.
func TestAplDeployedNamesWhatItFound(t *testing.T) {
	v := evaluateAplDeployed([]byte(`{"items":[{"metadata":{"name":"apl-operator","labels":{}}},{"metadata":{"name":"other","labels":{}}}]}`), nil)
	if v.Err == nil {
		t.Fatal("no version-bearing label must fail")
	}
	for _, want := range []string{"apl-operator", "other", chartLabel, appVersionLabel} {
		if !strings.Contains(v.Err.Error(), want) {
			t.Errorf("the failure must name %q so the reader can find the new label; got: %v", want, v.Err)
		}
	}
}

// The fallback exists because the chart writes both labels, and the verdict must
// SAY which one it read: "we could not collect" and "we read the other field" have
// nothing in common as remedies.
func TestAplDeployedFallsBackToAppVersion(t *testing.T) {
	v := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		appVersionLabel: clusterspec.BaselineAplChartVersion,
	}), nil)
	if v.Err != nil {
		t.Fatalf("the appVersion fallback must be usable: %v", v.Err)
	}
	if v.Source != appVersionLabel {
		t.Errorf("source = %q, want %q", v.Source, appVersionLabel)
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
		got := classifyAplDeployed(live, chartLabel)
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
	if got := classifyAplDeployed(major, chartLabel); got.Err == nil {
		t.Fatal("without the override a major apart must fail")
	} else if !strings.Contains(got.Err.Error(), clusterspec.AllowMajorDriftEnv) {
		t.Errorf("the failure must name the override, or the operator cannot find it: %v", got.Err)
	}

	t.Setenv(clusterspec.AllowMajorDriftEnv, "1")
	got := classifyAplDeployed(major, chartLabel)
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
	v := classifyAplDeployed("6.1.0", chartLabel)
	if v.Warn == "" {
		t.Fatal("a minor behind must warn")
	}
	for _, want := range []string{"6.1.0", clusterspec.BaselineAplChartVersion, "Linode owns the rollout"} {
		if !strings.Contains(v.Warn, want) {
			t.Errorf("warning must contain %q, got: %s", want, v.Warn)
		}
	}
}

// A RELEASE CANDIDATE IS AN ORDINARY CHART. The last "-" in "apl-v6.3.0-rc.1"
// opens "rc.1", which does not parse — so a last-dash scan declared the label
// unreadable and hard-failed this GATING lane on a cluster that was merely running
// an rc. clusterspec.AplSemver tolerates the suffix on purpose, and this must too.
func TestAplChartVersionFromLabelsToleratesPreRelease(t *testing.T) {
	cases := map[string]string{
		"apl-v6.2.1":       "v6.2.1",
		"apl-v6.3.0-rc.1":  "v6.3.0-rc.1",
		"apl-6.0.0":        "6.0.0",
		"some-chart-1.2.3": "1.2.3",
	}
	for label, want := range cases {
		got, src := aplChartVersionFromLabels(map[string]string{chartLabel: label})
		if got != want {
			t.Errorf("aplChartVersionFromLabels(%q) = %q, want %q", label, got, want)
		}
		if src != chartLabel {
			t.Errorf("source for %q = %q, want %q", label, src, chartLabel)
		}
	}
	// And an rc must reach a VERDICT, not the unparseable failure arm.
	v := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel:  aplOperatorName,
		chartLabel: "apl-" + clusterspec.BaselineAplChartVersion + "-rc.1",
	}), nil)
	if v.Err != nil {
		t.Errorf("an rc of the baseline must not fail the lane: %v", v.Err)
	}
}

// A SIBLING IN THE SAME NAMESPACE IS NOT APL-CORE. Any Deployment from any chart
// carries helm.sh/chart and app.kubernetes.io/version, so reading "the first item
// with a version label" reports a neighbour's version as apl-core's — and hard-fails
// a gating lane on a cluster that is perfectly in step. The neighbour here sorts
// first and is a MAJOR away, so an unfiltered read fails loudly.
func TestAplDeployedIgnoresSiblingDeployments(t *testing.T) {
	raw := []byte(`{"items":[` +
		`{"metadata":{"name":"aaa-other","labels":{"helm.sh/chart":"other-1.0.0","app.kubernetes.io/name":"other"}}},` +
		`{"metadata":{"name":"apl-operator","labels":{"helm.sh/chart":"apl-` + clusterspec.BaselineAplChartVersion + `","app.kubernetes.io/name":"apl-operator"}}}` +
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
	v := evaluateAplDeployed(deployJSON("other", map[string]string{
		chartLabel: "other-1.0.0", nameLabel: "other",
	}), nil)
	if v.Err == nil {
		t.Fatal("no apl-operator Deployment must FAIL — a filtered query that matches nothing is the vacuous pass this lane refuses")
	}
	if !strings.Contains(v.Err.Error(), "other") {
		t.Errorf("the failure must name what IS present, got: %v", v.Err)
	}
}

// THE FALLBACK IS A WEAKER SOURCE AND MUST NEVER BLOCK. app.kubernetes.io/version
// is the chart's appVersion; the baseline it is compared against is a CHART
// version. They are the same string for apl-core today and nothing upstream
// guarantees they stay coupled — so grading the fallback as if it were the chart
// version would hard-fail a gating lane on a healthy cluster the day they diverge.
func TestAplDeployedAppVersionFallbackNeverBlocks(t *testing.T) {
	v := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel:       aplOperatorName,
		appVersionLabel: "9.0.0", // a major apart: blocking, if it were the chart version
	}), nil)
	if v.Err != nil {
		t.Fatalf("a blocking-looking appVersion must be REPORTED, not failed: %v", v.Err)
	}
	if v.Warn == "" {
		t.Fatal("it must still warn — a weaker source is not a reason for silence")
	}
	for _, want := range []string{appVersionLabel, chartLabel, "not guaranteed"} {
		if !strings.Contains(v.Warn, want) {
			t.Errorf("the warning must say which label it read and why that is weaker (missing %q): %s", want, v.Warn)
		}
	}
	// The chart label, by contrast, is ground truth and DOES block.
	v = evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel:  aplOperatorName,
		chartLabel: "apl-9.0.0",
	}), nil)
	if v.Err == nil {
		t.Error("a major apart read from the CHART label must still fail — softening the fallback must not soften the real source")
	}
}

// AN UNPARSEABLE CHART LABEL MUST NOT BEAT A USABLE appVersion SITTING BESIDE IT.
// The fallback was consulted only when helm.sh/chart was ABSENT, so a mangled label
// hard-failed this gating lane while the answer was on the same object.
func TestAplDeployedFallsBackWhenTheChartLabelIsMangled(t *testing.T) {
	v := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel:       aplOperatorName,
		chartLabel:      "apl-garbage",
		appVersionLabel: clusterspec.BaselineAplChartVersion,
	}), nil)
	if v.Err != nil {
		t.Fatalf("a usable appVersion beside a mangled chart label must be used, not failed: %v", v.Err)
	}
	if v.Source != appVersionLabel {
		t.Errorf("source = %q, want %q so the reader knows which label answered", v.Source, appVersionLabel)
	}
	// With BOTH unusable there is genuinely no answer, and that still fails.
	v = evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel:       aplOperatorName,
		chartLabel:      "apl-garbage",
		appVersionLabel: "also-garbage",
	}), nil)
	if v.Err == nil {
		t.Error("two unreadable labels is still UNKNOWN, and unknown fails")
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
	behind := classifyAplDeployed("5.0.0", chartLabel)
	if behind.Err == nil {
		t.Fatal("a major behind must block")
	}
	if strings.Contains(behind.Err.Error(), "Upgrade llz to a release that targets this platform") {
		t.Error("that remedy is impossible for a cluster on an older major — no newer llz targets apl-core 5.x")
	}
	if !strings.Contains(behind.Err.Error(), "Linode owns the rollout") {
		t.Errorf("it must say who can actually move it, got: %v", behind.Err)
	}

	ahead := classifyAplDeployed("9.0.0", chartLabel)
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

// A LANE THAT HAS STOPPED GATING MUST SAY SO. Never blocking on the weaker source is
// deliberate, but it has a cost: if apl-core stops writing helm.sh/chart entirely
// this lane becomes warn-only forever and nobody is told the gate stopped gating.
func TestDegradedFallbackAnnouncesItself(t *testing.T) {
	v := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel:       aplOperatorName,
		appVersionLabel: "9.0.0",
	}), nil)
	if v.Err != nil {
		t.Fatalf("the weaker source must not block: %v", v.Err)
	}
	if !strings.HasPrefix(v.Warn, "DEGRADED") {
		t.Errorf("the degradation must lead, not be buried: %s", v.Warn)
	}
	if !strings.Contains(v.Warn, "not gating") {
		t.Errorf("it must say the gate is not gating: %s", v.Warn)
	}
}

// DEGRADED IS ABOUT THE SOURCE, NOT THE VERDICT. The banner hung off the major-drift
// arm alone, so if helm.sh/chart disappeared while drift was minor — or absent —
// the lane said "routine mid-rollout state", or nothing at all, and went warn-only
// forever without naming the degradation. Every arm reachable through the fallback
// must announce it.
func TestDegradedIsAnnouncedOnEveryFallbackArm(t *testing.T) {
	fallback := func(version string) AplDeployedVerdict {
		return evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
			nameLabel:       aplOperatorName,
			appVersionLabel: version,
		}), nil)
	}
	for _, tc := range []struct{ name, version string }{
		{"in agreement", clusterspec.BaselineAplChartVersion},
		{"minor drift", "6.1.0"},
		{"blocking drift", "9.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := fallback(tc.version)
			if v.Err != nil {
				t.Fatalf("the weaker source must never block: %v", v.Err)
			}
			if !strings.HasPrefix(v.Warn, "DEGRADED") {
				t.Errorf("every fallback arm must announce the degradation, got %q", v.Warn)
			}
			if !strings.Contains(v.Warn, chartLabel) {
				t.Errorf("it must name the label that went missing, got %q", v.Warn)
			}
		})
	}

	// ...and the authoritative source must NOT be labelled degraded.
	ok := evaluateAplDeployed(deployJSON("apl-operator", map[string]string{
		nameLabel:  aplOperatorName,
		chartLabel: "apl-" + clusterspec.BaselineAplChartVersion,
	}), nil)
	if strings.Contains(ok.Warn, "DEGRADED") {
		t.Errorf("a healthy authoritative read is not degraded, got %q", ok.Warn)
	}
}

// A STAGED MAJOR IS NOT A PATCH LAG. Both reach the permitted-drift arm — a
// minor/patch gap outright, and a MAJOR one once the override is set — and one
// sentence covered both, so a deliberately staged 7.0.0 against a v6.2.1 baseline
// read in the weekly check exactly like a point-release lag. The override suppresses
// the block, not the distance.
func TestStagedMajorReadsDifferentlyFromAPatchLag(t *testing.T) {
	t.Setenv(clusterspec.AllowMajorDriftEnv, "1")
	staged := classifyAplDeployed("7.0.0", chartLabel)
	if staged.Err != nil {
		t.Fatalf("the override must permit it: %v", staged.Err)
	}
	if !strings.Contains(staged.Warn, "MAJOR") || !strings.Contains(staged.Warn, clusterspec.AllowMajorDriftEnv) {
		t.Errorf("a staged major must say what it is and why it is not failing, got %q", staged.Warn)
	}
	if strings.Contains(staged.Warn, "routine mid-rollout") {
		t.Errorf("a major apart is not the routine mid-rollout state, got %q", staged.Warn)
	}

	lag := classifyAplDeployed("6.1.0", chartLabel)
	if !strings.Contains(lag.Warn, "routine mid-rollout") {
		t.Errorf("a minor lag IS the routine state, got %q", lag.Warn)
	}
	if strings.Contains(lag.Warn, clusterspec.AllowMajorDriftEnv) {
		t.Errorf("a patch lag has nothing to do with the override, got %q", lag.Warn)
	}
}

// AGREEMENT IS NOT DRIFT. The `::warning::` prefix was unconditional, so a cluster on
// exactly the baseline whose helm.sh/chart label had gone missing — Warn carrying only
// the DEGRADED banner — was annotated as having drifted. That is the "collection
// stopped" versus "we read a different field" conflation the banner argues against,
// reintroduced by the line that prints it.
func TestWarningLabelNamesWhatHappened(t *testing.T) {
	agree := classifyAplDeployed(clusterspec.BaselineAplChartVersion, appVersionLabel)
	if agree.Err != nil {
		t.Fatalf("agreement must not fail: %v", agree.Err)
	}
	if clusterspec.AplChartDriftOf(agree.Live) != clusterspec.AplChartDriftNone {
		t.Fatal("premise: the baseline must read as no drift")
	}
	if !strings.HasPrefix(agree.Warn, "DEGRADED") {
		t.Errorf("a fallback read still announces the degradation, got %q", agree.Warn)
	}

	// And a real gap still classifies as drift.
	lag := classifyAplDeployed("6.1.0", chartLabel)
	if clusterspec.AplChartDriftOf(lag.Live) == clusterspec.AplChartDriftNone {
		t.Error("premise: 6.1.0 must read as drift against the baseline")
	}
}
