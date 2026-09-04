package assertplatform

// liveaplversion.go implements `llz ci assert-apl-deployed-version` — the half of
// the apl-core version question that reads a CLUSTER.
//
// ── WHY THE SPEC-SIDE CHECK COULD NOT ANSWER THIS ────────────────────────────
//
// `assert-apl-version` (aplversion.go) resolves the version out of the SPEC:
// spec.cluster.bootstrap.aplChartVersion, or clusterspec.BaselineAplChartVersion
// when the pin is omitted. That is a statement about how the instance is
// CONFIGURED, and it was the only apl-core version signal this repo had.
//
// On Linode's MANAGED App Platform it is also, on its own, a fiction. Linode
// installs and owns apl-core: `apl_enabled` is a create-time boolean and the
// Linode API carries no version field at all — not settable, not readable — so
// nothing LLZ does moves the deployed version, and no amount of agreement between
// the spec and the baseline says anything about what is running. An instance whose
// platform Linode has rolled forward (or has not yet rolled forward) looks
// identical to one in lockstep. That is the audit-pipeline shape one more time:
// two values consistent with each other are not two correct values.
//
// ── GROUND TRUTH, NOT A PROXY ────────────────────────────────────────────────
//
// The version is read from the `helm.sh/chart` label on apl-core's own
// apl-operator Deployment. The chart writes it from `Chart.Name-Chart.Version`
// (templates/_helpers.tpl, "apl-operator.labels"), so the label IS the published
// chart version — the same string BaselineAplChartVersion carries.
//
// NOT THE CONTAINER IMAGE TAG, which is the obvious reading and is a proxy: the
// chart takes the operator image tag from `.Values.otomi.version | default
// .Chart.AppVersion`, so an install that sets otomi.version reports a version that
// is not the chart's. `app.kubernetes.io/version` (the chart's appVersion) is the
// fallback, and the verdict says which of the two it read — "collection stopped"
// and "we read a different field" have nothing in common as remedies.
//
// ── THE POLICY IS THE SPEC-SIDE POLICY, APPLIED TO REALITY ───────────────────
//
// The verdict runs clusterspec.AplChartDriftOf over the LIVE version rather than
// over a pin. One classifier, two inputs: a major apart in either direction is a
// version this llz release has not been tested against and fails; a minor or patch
// apart is the routine mid-rollout state and warns. Restating the thresholds here
// would be a second copy of the rule, and the two would diverge on the first bump.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// apl-core's operator namespace and Deployment name.
//
// LOCAL, AND NOT ALIASED TO bootstrapcluster's EXPORTED PAIR, though that is where
// the same two strings already live (prepare_apl_upgrade.go annotates this very
// Deployment). Importing them is architecturally refused, not merely awkward:
// `internal/extensions` packages must not import each other, and
// TestNoNewExtensionToExtensionImports fails the edge on sight. The sanctioned fix
// is to split the library half of bootstrapcluster down into internal/shared, which
// is a far wider change than two twelve-character constants justify.
//
// So the duplication stays and is PINNED INSTEAD — see the coupling assertion in
// live_apl_version_test.go, which reads bootstrapcluster's exported values and
// requires these to match. A test may cross that boundary where production code may
// not, so the rename-one-side failure this would otherwise invite still cannot land
// quietly.
const (
	aplOperatorNamespace = "apl-operator"
	aplOperatorName      = "apl-operator"
)

// chartLabel / appVersionLabel are the two version-bearing labels the chart's
// common-labels helper writes.
const (
	chartLabel      = "helm.sh/chart"
	appVersionLabel = "app.kubernetes.io/version"
	// nameLabel/aplOperatorName identify apl-core's own operator. The chart's
	// selectorLabels helper writes `app.kubernetes.io/name: apl-operator` as a
	// LITERAL, independent of the helm release name, so it identifies the workload
	// even where fullname is prefixed.
	nameLabel = "app.kubernetes.io/name"
)

// deployList is the sliver of `kubectl get deploy -o json` this lane reads.
type deployList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	} `json:"items"`
}

// aplChartVersionFromLabels extracts the chart version from one object's labels.
//
// `helm.sh/chart` is "<chart name>-<chart version>" — "apl-v6.2.1". The chart name
// is stripped by scanning the "-" positions LEFT TO RIGHT and taking the first
// suffix that parses, rather than by trimming a hardcoded "apl-" prefix: the label
// is truncated to 63 characters by the helper, and a name assumption would turn a
// renamed chart into a silent unparseable rather than a loud one.
//
// LEFT TO RIGHT, NOT THE LAST DASH, and a pre-release is exactly why. The last "-"
// in "apl-v6.3.0-rc.1" opens "rc.1", which does not parse — so a last-dash scan
// declared an ordinary release-candidate chart unreadable and hard-failed this
// gating lane, while clusterspec.AplSemver deliberately TOLERATES the suffix. The
// first parseable suffix is "v6.3.0-rc.1", which is the answer.
func aplChartVersionFromLabels(labels map[string]string) (version, source string) {
	if v := labels[chartLabel]; v != "" {
		for i := range len(v) {
			if v[i] != '-' || i+1 >= len(v) {
				continue
			}
			if _, _, _, ok := clusterspec.AplSemver(v[i+1:]); ok {
				return v[i+1:], chartLabel
			}
		}
		// PRESENT BUT NOT PARSEABLE, and the appVersion is tried before giving up: a
		// mangled `helm.sh/chart` sitting beside a perfectly good
		// app.kubernetes.io/version was hard-failing this gating lane while the answer
		// was on the same object. Only when neither yields anything usable does the
		// unparseable label become the verdict — returned rather than dropped so the
		// failure can PRINT it, because an unreadable label is a different problem
		// from an absent one.
		if av := labels[appVersionLabel]; av != "" {
			if _, _, _, ok := clusterspec.AplSemver(av); ok {
				return av, appVersionLabel
			}
		}
		return v, chartLabel
	}
	if v := labels[appVersionLabel]; v != "" {
		return v, appVersionLabel
	}
	return "", ""
}

// AplDeployedVerdict is the lane's decision.
type AplDeployedVerdict struct {
	// Live is the chart version read from the cluster, "" when none could be read.
	Live string
	// Source names the label it came from, for the failure message.
	Source string
	// Err is non-nil when the lane FAILS. Warnings are carried in Warn.
	Err  error
	Warn string
}

// evaluateAplDeployed is the whole judgement, pure over parsed input so every arm
// is testable without a cluster.
//
// FAILS CLOSED ON EVERY FORM OF "COULD NOT TELL". Zero deployments, no
// version-bearing label, an unparseable label — each is a failure, not an empty
// pass. A lane that reports success having read nothing looks exactly like the
// drift it exists to catch, and this one is READ-ONLY, so there is no cost to
// being loud.
func evaluateAplDeployed(raw []byte, readErr error) AplDeployedVerdict {
	if readErr != nil {
		return AplDeployedVerdict{Err: fmt.Errorf(
			"could not read the apl-operator Deployment in namespace %s, so the deployed apl-core version is UNKNOWN — "+
				"that is a failure, not a pass: %w", aplOperatorNamespace, readErr)}
	}
	var list deployList
	if err := json.Unmarshal(raw, &list); err != nil {
		return AplDeployedVerdict{Err: fmt.Errorf(
			"the apl-operator Deployment listing in namespace %s did not parse as JSON, so the deployed apl-core version is UNKNOWN: %w",
			aplOperatorNamespace, err)}
	}
	if len(list.Items) == 0 {
		return AplDeployedVerdict{Err: fmt.Errorf(
			"no Deployment at all in namespace %s — apl-core's operator is where the deployed chart version is legible, "+
				"so either the managed App Platform is not installed on this cluster or it has moved. Nothing was checked",
			aplOperatorNamespace)}
	}

	// APL-CORE'S OWN OPERATOR, SELECTED BY NAME, not "whichever Deployment in this
	// namespace carries a version label first". The namespace is not guaranteed to
	// hold only apl-core's Deployment, and any neighbour from a different chart
	// carries the very same helm.sh/chart and app.kubernetes.io/version labels — so
	// an unfiltered read reports a sibling's version as apl-core's and hard-fails a
	// gating lane on a cluster that is perfectly in step.
	//
	// The selector is app.kubernetes.io/name, a DIFFERENT label from the ones being
	// read. Selecting on the version label itself would derive the expected set from
	// the thing under test — the shape where a filtered query returns nothing and the
	// gate passes on the bug it exists to catch.
	var seen []string
	for _, it := range list.Items {
		seen = append(seen, it.Metadata.Name)
		if it.Metadata.Labels[nameLabel] != aplOperatorName && it.Metadata.Name != aplOperatorName {
			continue
		}
		v, src := aplChartVersionFromLabels(it.Metadata.Labels)
		if v == "" {
			continue
		}
		if _, _, _, ok := clusterspec.AplSemver(v); !ok {
			return AplDeployedVerdict{Live: v, Source: src, Err: fmt.Errorf(
				"%s/%s carries %s=%q, which is not a chart version this llz can parse — the deployed apl-core version is UNKNOWN",
				aplOperatorNamespace, it.Metadata.Name, src, v)}
		}
		return classifyAplDeployed(v, src)
	}

	// NAME WHAT IS PRESENT. The label being looked for may have been RENAMED, and
	// no amount of staring at the absent name reveals the new one.
	//
	// THE BLAST RADIUS OF THIS ARM IS EVERY ADOPTER AT ONCE, because the lane is
	// gating on the delivered scheduled health check — so the remedy has to be in the
	// message. The labels are not guessed: apl-core's chart writes them from its own
	// common-labels helper (templates/_helpers.tpl, "apl-operator.labels" — chart
	// v6.2.1 verified), and release-e2e runs this lane against a real MANAGED cluster
	// before any release carrying it can ship, which is what stands between a wrong
	// assumption here and an adopter's pipeline. If Linode's install ever stops
	// writing them, that run goes red first.
	sort.Strings(seen)
	return AplDeployedVerdict{Err: fmt.Errorf(
		"no Deployment named %[1]q (or labelled %[2]s=%[1]s) in namespace %[3]s carries %[4]s or %[5]s, "+
			"so the deployed apl-core version is UNKNOWN. Deployments present: %[6]s. "+
			"If the managed platform has stopped labelling its operator this lane cannot answer, and the fix is a NEW llz release that "+
			"reads whatever replaced them — `llz self-update && llz upgrade`. There is no per-instance opt-out to reach for: "+
			"LLZ_ALLOW_APL_CHART_MAJOR_DRIFT releases a major-version BLOCK, not an unreadable one, and the two delivered call sites "+
			"(the weekly platform job in llz-scheduled-checks.yml and the e2e assert-suite) are digest-locked, so editing them locally "+
			"fails `llz lint`. Until a release lands, disable that scheduled job",
		aplOperatorName, nameLabel, aplOperatorNamespace, chartLabel, appVersionLabel, strings.Join(seen, ", "))}
}

// classifyAplDeployed applies the SPEC-SIDE drift policy to the live version.
//
// THE BLOCK/ALLOW DECISION IS clusterspec.AplChartDriftBlocks, not a threshold
// restated here — which is what this was, and it was already wrong in one arm: it
// failed on major drift without consulting LLZ_ALLOW_APL_CHART_MAJOR_DRIFT, the
// override the spec-side gate has honoured all along. On managed App Platform that
// is the worst possible place to lose an escape hatch, because LINODE moves the
// version: the lane would have reddened `assert-suite` and the scheduled health
// check on a condition the operator could neither fix nor opt out of.
func classifyAplDeployed(live, source string) AplDeployedVerdict {
	v := AplDeployedVerdict{Live: live, Source: source}
	drift := clusterspec.AplChartDriftOf(live)

	// DEGRADED IS ABOUT THE SOURCE, NOT THE VERDICT, so it prefixes every arm reached
	// through the fallback — agreement included. Hanging it off the major-drift arm
	// alone would let the lane go warn-only forever, unnoticed, whenever the
	// authoritative label vanishes while drift happens to be small.
	//
	// WHY the fallback was used is deliberately not asserted: it is reached both when
	// the chart label is ABSENT and when it is PRESENT BUT UNPARSEABLE, and naming the
	// wrong one sends an operator hunting for a label that is sitting there and wrong.
	degraded := ""
	if source == appVersionLabel {
		degraded = fmt.Sprintf(
			"DEGRADED — this lane is not gating right now: %s is absent or unparseable on the %s Deployment in %s, so the version "+
				"was read from %s (the chart's appVersion), which is not guaranteed to equal the chart version. Treat a persisting "+
				"DEGRADED as a platform change llz needs to catch up with. ",
			chartLabel, aplOperatorName, aplOperatorNamespace, appVersionLabel)
	}

	if drift == clusterspec.AplChartDriftNone {
		// Even in agreement the authoritative label is gone, and saying so is the only
		// thing that stops the lane rotting unnoticed.
		v.Warn = strings.TrimSpace(degraded)
		return v
	}

	// THE FALLBACK MAY NEVER BLOCK. app.kubernetes.io/version is the chart's
	// appVersion, and the baseline it is graded against is a CHART version. For
	// apl-core the two are the same string today (Chart.yaml v6.2.1 declares both),
	// but nothing upstream guarantees they stay coupled — so a chart that ever
	// diverges them would hard-fail this gating lane on a perfectly healthy cluster.
	// The reading is still worth reporting; it is not worth failing on.
	if source == appVersionLabel && clusterspec.AplChartDriftBlocks(drift) {
		v.Warn = degraded + fmt.Sprintf(
			"It reports apl-core %s against the %s this llz release targets, far enough apart to block had the authoritative "+
				"label been readable — confirm by hand before treating it as drift",
			live, clusterspec.BaselineAplChartVersion)
		return v
	}

	if clusterspec.AplChartDriftBlocks(drift) {
		// THE REMEDY DEPENDS ON WHICH WAY THE GAP RUNS, and one blanket sentence got
		// it wrong half the time. "Upgrade llz to a release that targets this
		// platform" is impossible when the CLUSTER is the old one: no newer llz
		// targets apl-core 5.x. An instruction that cannot be followed is worse than
		// none — it spends the reader's time before they work out it is wrong.
		fix := "Upgrade llz to a release that targets this platform"
		if drift == clusterspec.AplChartDriftMajorBehind {
			fix = "This cluster is on a platform major that this llz release has left behind — Linode owns the rollout on " +
				"managed App Platform, so raise it with them, or pin this instance to an llz release that still targeted it"
		}
		v.Err = fmt.Errorf(
			"this cluster runs apl-core %s, a MAJOR apart from the %s this llz release targets — llz has not been tested against it. "+
				"Read %s from %s. %s, or set %s=1 to stage the move deliberately (Linode owns the rollout here, so this may not be "+
				"a version you chose)",
			live, clusterspec.BaselineAplChartVersion, source, aplOperatorNamespace, fix, clusterspec.AllowMajorDriftEnv)
		return v
	}

	// A STAGED MAJOR IS NOT A PATCH LAG. Both reach here — the shared predicate
	// permits a minor/patch gap outright, and a MAJOR one once AllowMajorDriftEnv is
	// set — and one sentence covered both, so a deliberately staged 7.0.0 against a
	// v6.2.1 baseline read in the weekly check exactly like a point-release lag. The
	// override suppresses the block, not the distance.
	if drift == clusterspec.AplChartDriftMajorBehind || drift == clusterspec.AplChartDriftMajorAhead {
		v.Warn = degraded + fmt.Sprintf(
			"this cluster runs apl-core %s, a MAJOR apart from the %s this llz release targets. It is not failing because %s is "+
				"set, which is a deliberate, time-boxed staging switch — llz has NOT been tested against this platform, so unset it "+
				"once the staged move is done rather than leaving it on",
			live, clusterspec.BaselineAplChartVersion, clusterspec.AllowMajorDriftEnv)
		return v
	}
	v.Warn = degraded + fmt.Sprintf(
		"this cluster runs apl-core %s and this llz release targets %s. Linode owns the rollout on managed App Platform "+
			"(the API has no version field), so this is the routine mid-rollout state and does not fail — "+
			"but it is the version llz was NOT tested against, and it is what to check first if something behaves oddly",
		live, clusterspec.BaselineAplChartVersion)
	return v
}

// assertAplDeployedVersion is the lane.
func assertAplDeployedVersion() error {
	raw, err := deps.Exec("kubectl", "-n", aplOperatorNamespace, "get", "deploy", "-o", "json")
	v := evaluateAplDeployed(raw, err)
	if v.Err != nil {
		return v.Err
	}
	if v.Warn != "" {
		// THE PREFIX MUST NAME WHAT HAPPENED. Applied unconditionally it labelled a
		// cluster in EXACT AGREEMENT as having drifted, whenever its helm.sh/chart
		// label had gone missing and the Warn carried only the DEGRADED banner —
		// exactly the "collection stopped" versus "we read a different field"
		// conflation the banner's own comment argues against, reintroduced one layer
		// up by the line that prints it.
		label := "apl-core version drift"
		if clusterspec.AplChartDriftOf(v.Live) == clusterspec.AplChartDriftNone {
			label = "apl-core version lane degraded"
		}
		fmt.Printf("::warning::%s: %s\n", label, v.Warn)
		return nil
	}
	fmt.Printf("deployed apl-core %s matches the version this llz release targets (%s), read from %s on a Deployment in %s.\n",
		v.Live, clusterspec.BaselineAplChartVersion, v.Source, aplOperatorNamespace)
	return nil
}
