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
// the spec and the baseline says anything about what is running. That is the
// audit-pipeline shape one more time: two values consistent with each other are
// not two correct values.
//
// ── READ THE IMAGE TAG. NOT THE CHART LABELS. ────────────────────────────────
//
// The version is the apl-operator container's IMAGE TAG, because apl-core sets it
// from the platform version and nothing else here does:
//
//	values/apl-operator/apl-operator.gotmpl:  {{- $version := $v.otomi.version }}
//	                                          tag: {{ $version }}
//
// `otomi.version` IS the platform version — the single knob apl-core's own
// runtime-upgrade state machine reads and writes.
//
// This lane originally read `helm.sh/chart`, with `app.kubernetes.io/version` as a
// fallback, and BOTH ARE WRONG — not stale, not degraded, wrong by construction.
// The Deployment is rendered by the apl-operator SUB-chart, so its common-labels
// helper interpolates that sub-chart's own coordinates:
//
//	charts/apl-operator/Chart.yaml:  version: 0.2.0     → helm.sh/chart: apl-operator-0.2.0
//	                                 appVersion: 1.16.0 → app.kubernetes.io/version: "1.16.0"
//
// Those are constants of the operator's packaging. They do not move when the
// platform moves, and they never equalled BaselineAplChartVersion in any state —
// not at install, not after reconcile. A real managed cluster running v6.2.1 read
// as "apl-core 0.2.0, a MAJOR apart", hard-failing this gating lane against a
// perfectly in-step platform.
//
// The mistake that shipped it is worth naming, because the file it points at is
// genuinely the right one: `apl-operator.labels` does live in
// charts/apl-operator/templates/_helpers.tpl. What went unchecked was the sibling
// Chart.yaml supplying `.Chart.Version` to it — the UMBRELLA chart's version was
// assumed to reach a sub-chart's labels, and it does not.
//
// So there is NO SECOND SOURCE and deliberately no fallback: the two labels that
// look like one are the bug. Unreadable is a hard failure.
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

// nameLabel/aplOperatorName identify apl-core's own operator. The chart's
// selectorLabels helper writes `app.kubernetes.io/name: apl-operator` as a LITERAL,
// independent of the helm release name, so it identifies the workload even where
// fullname is prefixed.
//
// A LABEL IS STILL THE RIGHT SELECTOR even though no label is a trustworthy VERSION
// source: selecting on identity and reading the version elsewhere is the point.
const nameLabel = "app.kubernetes.io/name"

// containerName is the operator container inside that Deployment. apl-core's
// template names it `{{ .Chart.Name }}`, which is the same "apl-operator" string.
const containerName = "apl-operator"

// imageTagSource names where the version came from, for the messages. There is one
// source, and it is still printed on every verdict: "collection stopped" and "we
// read a different field" have nothing in common as remedies, and the reader cannot
// tell which happened unless the lane says where it looked.
const imageTagSource = "the apl-operator container image tag"

// deployList is the sliver of `kubectl get deploy -o json` this lane reads.
type deployList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	} `json:"items"`
}

// imageTag returns the tag from a container image reference, or "" when it carries
// none.
//
// The DIGEST is stripped first, because "repo/apl-core:v6.2.1@sha256:…" is a legal
// pinned reference and a naive last-colon scan reads the digest hex as the version.
// A digest-ONLY reference has no tag and yields "" — unreadable, which fails closed
// rather than guessing.
//
// The tag separator must come AFTER the last "/", or a registry port is mistaken
// for one: "registry.example:5000/linode/apl-core" would otherwise report "5000".
func imageTag(image string) string {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		image = image[:i]
	}
	colon := strings.LastIndex(image, ":")
	if colon < 0 || colon < strings.LastIndex(image, "/") {
		return ""
	}
	return image[colon+1:]
}

// AplDeployedVerdict is the lane's decision.
type AplDeployedVerdict struct {
	// Live is the version read from the cluster, "" when none could be read.
	Live string
	// Source names where it came from, for the failure message.
	Source string
	// Err is non-nil when the lane FAILS. Warnings are carried in Warn.
	Err  error
	Warn string
}

// evaluateAplDeployed is the whole judgement, pure over parsed input so every arm
// is testable without a cluster.
//
// FAILS CLOSED ON EVERY FORM OF "COULD NOT TELL". Zero deployments, no operator
// container, an untagged or unparseable image — each is a failure, not an empty
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
			"no Deployment at all in namespace %s — apl-core's operator is where the deployed platform version is legible, "+
				"so either the managed App Platform is not installed on this cluster or it has moved. Nothing was checked",
			aplOperatorNamespace)}
	}

	// APL-CORE'S OWN OPERATOR, SELECTED BY NAME, not "whichever Deployment in this
	// namespace has a container first". The namespace is not guaranteed to hold only
	// apl-core's Deployment, and a neighbour from a different chart would report its
	// own image tag as apl-core's, hard-failing a gating lane on a cluster that is
	// perfectly in step.
	//
	// The selector is app.kubernetes.io/name, a DIFFERENT field from the one being
	// read. Selecting on the image itself would derive the expected set from the
	// thing under test — the shape where a filtered query returns nothing and the
	// gate passes on the bug it exists to catch.
	var seen []string
	for _, it := range list.Items {
		seen = append(seen, it.Metadata.Name)
		if it.Metadata.Labels[nameLabel] != aplOperatorName && it.Metadata.Name != aplOperatorName {
			continue
		}

		// The container is chosen BY NAME, with a single-container Deployment as the
		// only relaxation. A sidecar (a service mesh injects one without asking) is
		// otherwise a coin toss over which image gets read.
		cs := it.Spec.Template.Spec.Containers
		image := ""
		for _, c := range cs {
			if c.Name == containerName {
				image = c.Image
				break
			}
		}
		if image == "" && len(cs) == 1 {
			image = cs[0].Image
		}
		if image == "" {
			var names []string
			for _, c := range cs {
				names = append(names, c.Name)
			}
			return AplDeployedVerdict{Err: fmt.Errorf(
				"the %s/%s Deployment has no container named %q, so the deployed apl-core version is UNKNOWN. Containers present: %s",
				aplOperatorNamespace, it.Metadata.Name, containerName, strings.Join(names, ", "))}
		}

		tag := imageTag(image)
		if tag == "" {
			return AplDeployedVerdict{Err: fmt.Errorf(
				"the %s/%s operator image %q carries no tag, so the deployed apl-core version is UNKNOWN — a digest-pinned image "+
					"cannot say which platform version it is",
				aplOperatorNamespace, it.Metadata.Name, image)}
		}

		// A NON-SEMVER TAG IS A REAL apl-core STATE, NOT A MALFORMED READ. `otomi.version`
		// accepts a branch name — values-schema.yaml allows `[a-zA-Z]+[a-zA-Z0-9-]` beside
		// the semver form, and the chart keys pullPolicy off exactly that distinction
		// (`$isSemver := regexMatch "^[0-9.]+" $version` → Always vs IfNotPresent). So
		// "main" means a floating dev install, which llz cannot grade against a
		// baseline. It fails closed and NAMES the tag, rather than reporting drift it
		// did not measure.
		if _, _, _, ok := clusterspec.AplSemver(tag); !ok {
			return AplDeployedVerdict{Live: tag, Source: imageTagSource, Err: fmt.Errorf(
				"%s/%s runs operator image %q, whose tag %q is not a version this llz can compare against %s — apl-core allows a "+
					"branch name in otomi.version, so this is most likely a floating (non-release) platform install. The deployed "+
					"apl-core version is UNKNOWN",
				aplOperatorNamespace, it.Metadata.Name, image, tag, clusterspec.BaselineAplChartVersion)}
		}
		return classifyAplDeployed(tag)
	}

	// NAME WHAT IS PRESENT. The thing being looked for may have been RENAMED, and no
	// amount of staring at the absent name reveals the new one.
	//
	// THE BLAST RADIUS OF THIS ARM IS EVERY ADOPTER AT ONCE, because the lane is
	// gating on the delivered scheduled health check — so the remedy has to be in the
	// message. release-e2e runs this lane against a real MANAGED cluster before any
	// release carrying it can ship, which is what stands between a wrong assumption
	// here and an adopter's pipeline. It is also what caught the label version of
	// this lane; the assumption that shipped was never exercised against a cluster.
	sort.Strings(seen)
	return AplDeployedVerdict{Err: fmt.Errorf(
		"no Deployment named %[1]q (or labelled %[2]s=%[1]s) in namespace %[3]s, "+
			"so the deployed apl-core version is UNKNOWN. Deployments present: %[4]s. "+
			"If the managed platform has stopped shipping its operator under that name this lane cannot answer, and the fix is a NEW "+
			"llz release that reads whatever replaced it — `llz self-update && llz upgrade`. There is no per-instance opt-out to reach "+
			"for: %[5]s releases a major-version BLOCK, not an unreadable one, and the two delivered call sites (the weekly platform "+
			"job in llz-scheduled-checks.yml and the e2e assert-suite) are digest-locked, so editing them locally fails `llz lint`. "+
			"Until a release lands, disable that scheduled job",
		aplOperatorName, nameLabel, aplOperatorNamespace, strings.Join(seen, ", "), clusterspec.AllowMajorDriftEnv)}
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
func classifyAplDeployed(live string) AplDeployedVerdict {
	v := AplDeployedVerdict{Live: live, Source: imageTagSource}
	drift := clusterspec.AplChartDriftOf(live)

	if drift == clusterspec.AplChartDriftNone {
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
				"Read from %s in namespace %s. %s, or set %s=1 to stage the move deliberately (Linode owns the rollout here, so this "+
				"may not be a version you chose)",
			live, clusterspec.BaselineAplChartVersion, imageTagSource, aplOperatorNamespace, fix, clusterspec.AllowMajorDriftEnv)
		return v
	}

	// A STAGED MAJOR IS NOT A PATCH LAG. Both reach here — the shared predicate
	// permits a minor/patch gap outright, and a MAJOR one once AllowMajorDriftEnv is
	// set — and one sentence covered both, so a deliberately staged 7.0.0 against a
	// v6.2.1 baseline read in the weekly check exactly like a point-release lag. The
	// override suppresses the block, not the distance.
	if drift == clusterspec.AplChartDriftMajorBehind || drift == clusterspec.AplChartDriftMajorAhead {
		v.Warn = fmt.Sprintf(
			"this cluster runs apl-core %s, a MAJOR apart from the %s this llz release targets. It is not failing because %s is "+
				"set, which is a deliberate, time-boxed staging switch — llz has NOT been tested against this platform, so unset it "+
				"once the staged move is done rather than leaving it on",
			live, clusterspec.BaselineAplChartVersion, clusterspec.AllowMajorDriftEnv)
		return v
	}
	v.Warn = fmt.Sprintf(
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
		fmt.Printf("::warning::apl-core version drift: %s\n", v.Warn)
		return nil
	}
	fmt.Printf("deployed apl-core %s matches the version this llz release targets (%s), read from %s in namespace %s.\n",
		v.Live, clusterspec.BaselineAplChartVersion, v.Source, aplOperatorNamespace)
	return nil
}
