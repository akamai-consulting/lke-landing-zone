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
// The Deployment is rendered by the apl-operator SUB-chart, whose common-labels
// helper interpolates that SUB-chart's coordinates, not the umbrella chart's:
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
// So NEITHER LABEL may be a fallback: the two that look like one are the bug, and
// either in the source position reintroduces it. Unreadable is a hard failure.
//
// (apl-core does keep one other version record — the `otomi-status` ConfigMap in
// namespace `otomi`, carrying version/deployingVersion/deployingTag. It is not
// consulted here: one source that is right beats two that must be reconciled, and
// a second reader is a second thing to keep true. It is the place to look if the
// image tag ever stops being legible.)
//
// ── THE COUPLING THAT LETS AN IMAGE TAG BE GRADED AGAINST A CHART VERSION ────
//
// THIS LANE BLOCKS, so the assumption under the comparison has to be stated — and
// checked, which is how the reading it replaced went wrong. The tag is
// `otomi.version`; the baseline it is graded against,
// clusterspec.BaselineAplChartVersion, is a CHART version.
//
// VERIFIED against the published index (https://linode.github.io/apl-core), all 77
// `apl` entries: `version` and `appVersion` are the same release string on every
// one, differing only by the leading "v" apl-core adopted at 6.1.0 (`6.0.0` /
// `v6.0.0`), which clusterspec.AplSemver normalises away. apl-core cuts one version
// per release and stamps it into the chart, the appVersion and the image tag alike,
// so grading a tag against a chart-version baseline compares like with like.
//
// Nothing upstream GUARANTEES that continues. The previous code met that risk by
// refusing to block on its weaker source; this one blocks, so if apl-core ever
// versions its image independently of its chart, this lane goes red on healthy
// clusters. That is a deliberate trade: the failure is loud, arrives on the release
// that introduces it (release-e2e runs this against a real managed cluster before
// any release ships), and is fixed by teaching this file the new mapping — whereas
// the old never-block posture failed silently and shipped a wrong reading for
// months. `implausibleMajor` below is the cheap half of the defence, catching the
// decoupling that matters most: the tag ceasing to be a platform version at all.
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

// containerName is the operator container inside that Deployment (apl-core's
// template names it `{{ .Chart.Name }}`, the same "apl-operator" string).
//
// A THIRD CONSTANT RATHER THAN AN ALIAS of aplOperatorName, because an upstream
// rename of the container alone must be able to move independently of the
// Deployment's name. Selecting by it is what stops an injected sidecar — a service
// mesh adds one without asking — from having its own image tag read as the
// platform version.
const containerName = "apl-operator"

// aplCoreImageName is the last path element of apl-core's image, invariant across
// installs: `charts/apl-operator/values.yaml` ships `docker.io/linode/apl-core`,
// and the only override renders `<registry>/docker/linode/apl-core`. It gates the
// sole-container relaxation below, so the rescue path cannot read a foreign image.
const aplCoreImageName = "apl-core"

// imageTagSource names where the version came from. It is set on EVERY verdict,
// including the failures, and printed by the arms that have room for it: naming the
// source is what made the sub-chart-label bug diagnosable from a CI log alone.
const imageTagSource = "the apl-operator container image tag"

// deployContainer / deployItem / deployList are the sliver of
// `kubectl get deploy -o json` this lane reads.
type deployContainer struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type deployItem struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []deployContainer `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

type deployList struct {
	Items []deployItem `json:"items"`
}

// imageRepository returns an image reference with its tag and digest stripped.
func imageRepository(image string) string {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		image = image[:i]
	}
	if c := strings.LastIndex(image, ":"); c >= 0 && c > strings.LastIndex(image, "/") {
		image = image[:c]
	}
	return image
}

// isAplCoreImage reports whether a reference names apl-core's own image, whatever
// registry or mirror it is pulled through.
func isAplCoreImage(image string) bool {
	repo := imageRepository(image)
	return repo == aplCoreImageName || strings.HasSuffix(repo, "/"+aplCoreImageName)
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

// oldestAplMajor is the lowest major apl-core version any llz release has targeted,
// derived from clusterspec.AplBaselineHistory rather than written down, so it stays
// true as the history grows. Returns -1 when the history yields nothing parseable.
func oldestAplMajor() int {
	oldest := -1
	for _, b := range clusterspec.AplBaselineHistory {
		m, _, _, ok := clusterspec.AplSemver(b)
		if !ok {
			continue
		}
		if oldest < 0 || m < oldest {
			oldest = m
		}
	}
	return oldest
}

// implausibleMajor reports whether a tag parses as a version that PREDATES every
// apl-core release llz has ever targeted — which means it is not a platform version
// at all.
//
// It exists because three different faults all produce a plausible-looking semver
// that would otherwise be graded as "a major behind" and told to raise a rollout
// with Linode:
//
//   - `otomi.version` unset. The chart renders `tag:` null and Helm's
//     `default .Chart.AppVersion` fires, so the tag becomes the operator sub-chart's
//     appVersion, 1.16.0 — the same sub-chart constant this lane exists to stop
//     reading, arriving by a new route.
//   - a foreign container read through the sole-container relaxation (istio's
//     1.20.0, say), for a Deployment that has been relabelled.
//   - apl-core decoupling its image version from its chart version.
//
// All three are "this is not the platform version", not "the platform is old", and
// they need a different remedy than the drift arms give.
func implausibleMajor(tag string) bool {
	maj, _, _, ok := clusterspec.AplSemver(tag)
	if !ok {
		return false
	}
	oldest := oldestAplMajor()
	return oldest >= 0 && maj < oldest
}

// unreadableRemedy is the paragraph every "llz cannot read the version" failure
// carries.
//
// It exists because the arms that report an unreadable platform all reach the same
// audience — every adopter's weekly scheduled check, which has no continue-on-error
// — and all have the same fix. Only the no-matching-Deployment arm used to say so;
// the rest failed a fleet-wide gate while naming no way out, which is how a gate
// gets switched off.
func unreadableRemedy() string {
	return fmt.Sprintf(
		" If the managed platform has changed shape this lane cannot answer, and the fix is a NEW llz release that reads "+
			"whatever replaced it — `llz self-update && llz upgrade`. There is no per-instance opt-out to reach for: %s "+
			"releases a major-version BLOCK, not an unreadable one, and the two delivered call sites (the weekly platform "+
			"job in llz-scheduled-checks.yml and the e2e assert-suite) are digest-locked, so editing them locally fails "+
			"`llz lint`. Until a release lands, disable that scheduled job",
		clusterspec.AllowMajorDriftEnv)
}

// tagFailure says WHY a reference yielded no tag. imageTag collapses three states
// into "", and they point at different subsystems: a digest pin is a deliberate
// deployment choice, a missing tag is a malformed reference, and a bare
// registry-host reference is a registry problem. Asserting "digest-pinned" for all
// three sent readers of a private-registry install to the wrong place.
func tagFailure(image string) string {
	stripped := image
	if i := strings.LastIndex(image, "@"); i >= 0 {
		stripped = image[:i]
		if strings.LastIndex(stripped, ":") <= strings.LastIndex(stripped, "/") {
			return "it is pinned by digest alone, and a digest does not say which platform version it carries"
		}
	}
	if strings.HasSuffix(stripped, ":") {
		return "its tag is empty"
	}
	return "it names no tag at all"
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

// operatorImage picks the operator container's image out of one Deployment, or
// returns the failure explaining why it could not.
//
// EACH FAILURE IS ITS OWN ARM because the remedies differ, and a message that
// misdescribes the state sends the reader hunting for the wrong thing. "No
// containers at all" is an output-shape change; "no container by that name" is a
// rename; "found it, image empty" is a partial apply. Collapsing them produced
// `has no container named "apl-operator" … Containers present: apl-operator`,
// which denies the thing it then prints.
func operatorImage(deploy string, cs []deployContainer) (string, *AplDeployedVerdict) {
	fail := func(format string, a ...any) (string, *AplDeployedVerdict) {
		return "", &AplDeployedVerdict{Source: imageTagSource, Err: fmt.Errorf(format, a...)}
	}
	if len(cs) == 0 {
		return fail("the %s/%s Deployment declares no containers at all, so the deployed apl-core version is UNKNOWN — "+
			"the shape of the kubectl output this lane parses may have changed.%s", aplOperatorNamespace, deploy, unreadableRemedy())
	}
	var names []string
	for _, c := range cs {
		names = append(names, c.Name)
		if c.Name != containerName {
			continue
		}
		if c.Image == "" {
			return fail("the %s/%s container %q carries no image, so the deployed apl-core version is UNKNOWN.%s",
				aplOperatorNamespace, deploy, containerName, unreadableRemedy())
		}
		return c.Image, nil
	}

	// THE ONE RELAXATION, GATED ON THE REPOSITORY. A sole container is unambiguous
	// enough to survive an upstream rename of the container — but only if it is
	// actually apl-core. Ungated, this read a lone injected sidecar's tag as the
	// platform version (istio 1.20.0 → "a MAJOR apart, raise it with Linode"), which
	// is the container-level form of the very bug this lane was rewritten to fix.
	if len(cs) == 1 && isAplCoreImage(cs[0].Image) {
		return cs[0].Image, nil
	}
	return fail("the %s/%s Deployment has no container named %q running an %s image, so the deployed apl-core version is "+
		"UNKNOWN. Containers present: %s.%s",
		aplOperatorNamespace, deploy, containerName, aplCoreImageName, strings.Join(names, ", "), unreadableRemedy())
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
	//
	// A CANDIDATE THAT CANNOT ANSWER DOES NOT END THE SCAN. The selector accepts a
	// match by name OR by label, so a stale Deployment literally named `apl-operator`
	// can sit beside the renamed real one that carries the label — and returning the
	// first candidate's failure would report "unreadable" while the answer sat in the
	// next item. The first failure is remembered and used only if nothing better turns up.
	var seen []string
	var held *AplDeployedVerdict
	for _, it := range list.Items {
		seen = append(seen, it.Metadata.Name)
		if it.Metadata.Labels[nameLabel] != aplOperatorName && it.Metadata.Name != aplOperatorName {
			continue
		}
		image, bad := operatorImage(it.Metadata.Name, it.Spec.Template.Spec.Containers)
		if bad != nil {
			if held == nil {
				held = bad
			}
			continue
		}

		tag := imageTag(image)
		if tag == "" {
			if held == nil {
				held = &AplDeployedVerdict{Source: imageTagSource, Err: fmt.Errorf(
					"the %s/%s operator image %q carries no usable tag (%s), so the deployed apl-core version is UNKNOWN.%s",
					aplOperatorNamespace, it.Metadata.Name, image, tagFailure(image), unreadableRemedy())}
			}
			continue
		}

		// A NON-SEMVER TAG IS A REAL apl-core STATE, NOT A MALFORMED READ.
		// `otomi.version` accepts a branch name: values-schema.yaml allows
		// `[a-zA-Z]+[a-zA-Z0-9-]` beside the semver form, DEFAULTS THE FIELD TO
		// "latest", and apl-core's own fixtures ship `version: main`. So a
		// non-semver tag means a floating (non-release) install, which llz cannot
		// grade against a baseline. It fails closed and NAMES the tag, rather than
		// reporting drift it did not measure.
		//
		// Whether Linode's managed installer ALWAYS pins a release version is not
		// something llz verifies, and this arm has no per-instance override — so if
		// managed ever ships "latest" this goes red fleet-wide. That is why the
		// remedy paragraph is attached: the fix is a new llz release, not a dial the
		// adopter can turn.
		if _, _, _, ok := clusterspec.AplSemver(tag); !ok {
			return AplDeployedVerdict{Live: tag, Source: imageTagSource, Err: fmt.Errorf(
				"%s/%s runs operator image %q, whose tag %q is not a version this llz can compare against %s — apl-core allows a "+
					"branch name in otomi.version, so this is most likely a floating (non-release) platform install. The deployed "+
					"apl-core version is UNKNOWN.%s",
				aplOperatorNamespace, it.Metadata.Name, image, tag, clusterspec.BaselineAplChartVersion, unreadableRemedy())}
		}
		if implausibleMajor(tag) {
			return AplDeployedVerdict{Live: tag, Source: imageTagSource, Err: fmt.Errorf(
				"%s/%s runs operator image %q, whose tag %q predates every apl-core release llz has targeted (the oldest is %s), "+
					"so it is not a platform version at all rather than an old one. The usual cause is otomi.version being unset, "+
					"which makes the chart fall back to the apl-operator sub-chart's own appVersion; a foreign container in this "+
					"Deployment reads the same way. The deployed apl-core version is UNKNOWN.%s",
				aplOperatorNamespace, it.Metadata.Name, image, tag, clusterspec.AplBaselineHistory[0], unreadableRemedy())}
		}
		return classifyAplDeployed(tag)
	}
	if held != nil {
		return *held
	}

	// NAME WHAT IS PRESENT. The thing being looked for may have been RENAMED, and no
	// amount of staring at the absent name reveals the new one.
	//
	// THE BLAST RADIUS OF THIS ARM IS EVERY ADOPTER AT ONCE, because the lane is
	// gating on the delivered scheduled health check — so the remedy has to be in the
	// message. release-e2e runs this lane against a real MANAGED cluster before any
	// release carrying it can ship, which is what stands between a wrong assumption
	// here and an adopter's pipeline.
	sort.Strings(seen)
	return AplDeployedVerdict{Source: imageTagSource, Err: fmt.Errorf(
		"no Deployment named %[1]q (or labelled %[2]s=%[1]s) in namespace %[3]s, "+
			"so the deployed apl-core version is UNKNOWN. Deployments present: %[4]s.%[5]s",
		aplOperatorName, nameLabel, aplOperatorNamespace, strings.Join(seen, ", "), unreadableRemedy())}
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

	// THE SAFETY IS LOCAL, not left to the caller's guards. AplChartDriftOf answers
	// DriftNone for an empty version and Unparseable for a malformed one, and both
	// fall through the arms below to a silent pass or a "routine mid-rollout"
	// warning — a vacuous green from a function whose whole job is to refuse one.
	// evaluateAplDeployed cannot reach either today; nothing keeps a future caller
	// from doing so.
	if live == "" {
		v.Err = fmt.Errorf("no apl-core version was read, so the deployed version is UNKNOWN — that is a failure, not a pass")
		return v
	}
	drift := clusterspec.AplChartDriftOf(live)
	if drift == clusterspec.AplChartDriftUnparseable {
		v.Err = fmt.Errorf("apl-core version %q does not parse, so the deployed version is UNKNOWN", live)
		return v
	}

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
