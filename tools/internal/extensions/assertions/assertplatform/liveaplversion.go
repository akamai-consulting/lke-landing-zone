package assertplatform

// liveaplversion.go implements `llz ci assert-apl-deployed-version` — the half of
// the apl-core version question that reads a CLUSTER.
//
// The SPEC-side check (aplversion.go) resolves the version from
// spec.cluster.bootstrap.aplChartVersion, which on managed App Platform is a
// statement about configuration and nothing else: Linode installs and owns
// apl-core, `apl_enabled` is a create-time boolean, and the API carries no version
// field. The spec and the baseline can agree perfectly while the cluster runs
// something else — two values consistent with each other are not two correct
// values.
//
// ── READ THE IMAGE TAG. NOT THE CHART LABELS. ────────────────────────────────
//
// Two charts write the one apl-operator Deployment: the published `apl` chart
// installs it labelled apl-v6.2.1 / v6.2.1, then apl-core's own charts/apl-operator
// release REPLACES it (argocd Replace=true) and relabels from ITS Chart.yaml —
// apl-operator-0.2.0 / 1.16.0.
//
// So the labels carry the platform version EXACTLY ONCE, in a window no check runs
// in, and the operator chart's packaging constants for the rest of the cluster's
// life; a healthy v6.2.1 cluster reads as 0.2.0. NEITHER MAY BE A FALLBACK — a
// source correct only before the platform first reconciles is worse than none,
// because it is right on a fresh cluster and wrong on every real one.
//
// The image tag survives the swap: both charts render it from the platform version,
// the reconciled one from `otomi.version`
// (values/apl-operator/apl-operator.gotmpl: `tag: {{ $version }}`). apl-core also
// keeps otomi-status in ns `otomi` — the place to look if the tag stops being
// legible.
//
// GRADING A TAG AGAINST A CHART BASELINE compares like with like: across all 77
// published `apl` entries `version` and `appVersion` are the same release string,
// differing only by the leading "v" adopted at 6.1.0, which AplSemver normalises
// away. Nothing upstream guarantees that continues — if apl-core ever versions its
// image independently this lane reddens on healthy clusters, a deliberate trade for
// a failure that is loud and arrives on the release that introduces it.
// `implausibleMajor` catches the decoupling that matters most: the tag ceasing to
// be a platform version at all.
//
// The drift POLICY is clusterspec.AplChartDriftOf over the live version rather than
// a pin — one classifier, two inputs. Restating the thresholds here would be a
// second copy that diverges on the first bump.

import (
	"encoding/json"
	"fmt"
	"os"
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

// containerName is the operator container inside that Deployment. Both charts that
// write it agree on the name: the published `apl` chart hardcodes `- name:
// apl-operator`, and charts/apl-operator renders `{{ .Chart.Name }}`, which is the
// same string there.
//
// A THIRD CONSTANT RATHER THAN AN ALIAS of aplOperatorName, so an upstream rename
// of the container alone can move independently of the Deployment's name.
const containerName = "apl-operator"

// aplCoreImageName is the last path element of apl-core's image. THE SUFFIX AND NOT
// THE WHOLE REFERENCE: the registry differs by install (managed mirror, docker.io,
// the LKE override) but all three end the same way, so matching it admits any mirror
// an adopter pulls through while still refusing a foreign image.
const aplCoreImageName = "apl-core"

// imageTagSource names where the version came from. Set on every verdict, failures
// included, so a wrong source is diagnosable from a CI log alone.
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

// aplCoreLowestPublishedMajor is the lowest MAJOR apl-core has ever published: the
// chart index at https://linode.github.io/apl-core carries majors 3, 4, 5 and 6,
// and this repo's own docs discuss running 5.x. A tag below it is not a platform
// version at all.
//
// APL-CORE'S HISTORY, NOT LLZ'S. Deriving this from AplBaselineHistory — whose
// lowest major is 6 — collapsed it onto exactly the condition
// AplChartDriftMajorBehind fires on. Every genuinely old platform was then reported
// as "not a version", and LLZ_ALLOW_APL_CHART_MAJOR_DRIFT went INERT for the one
// case it exists for: a managed cluster on an older major, which the adopter can
// neither fix nor revert.
//
// A constant rather than a derivation, because this floor only moves if apl-core
// publishes something OLDER than 3.x, which released history cannot do.
const aplCoreLowestPublishedMajor = 3

// implausibleMajor reports whether a tag parses as a version apl-core has never
// published — not a platform version at all, as opposed to an old one.
//
// Two faults produce a plausible-looking semver that would otherwise be graded as
// drift and told to raise a rollout with Linode: `otomi.version` unset (the chart's
// `default .Chart.AppVersion` fires and the tag becomes the operator chart's
// appVersion, 1.16.0), and a foreign container read through the sole-container
// relaxation. Both mean "this is not the platform version", and need a different
// remedy than the drift arms give. An OLD major must fall through to those arms,
// override included.
func implausibleMajor(tag string) bool {
	maj, _, _, ok := clusterspec.AplSemver(tag)
	return ok && maj < aplCoreLowestPublishedMajor
}

// unreadableRemedy is the paragraph every "llz cannot read the version" failure
// carries.
//
// Every arm that reports an unreadable platform reaches the same audience — every
// adopter's weekly scheduled check, which has no continue-on-error — and shares one
// fix. A fleet-wide gate that names no way out is a gate that gets switched off.
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
// three would send a private-registry install to the wrong one.
func tagFailure(image string) string {
	stripped := image
	if i := strings.LastIndex(image, "@"); i >= 0 {
		stripped = image[:i]
	}
	tagged := strings.LastIndex(stripped, ":") > strings.LastIndex(stripped, "/")
	switch {
	case tagged && strings.HasSuffix(stripped, ":"):
		return "its tag is empty"
	case tagged:
		// Only reachable if imageTag stops agreeing with this function; answering
		// "no tag at all" for a reference that HAS one would be a wrong answer
		// waiting on that change.
		return "its tag could not be read"
	case strings.Contains(image, "@"):
		return "it is pinned by digest alone, and a digest does not say which platform version it carries"
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
		// CHECKED ON THIS PATH TOO, not only on the relaxation below: name-matching is
		// not identity. Any chart called apl-operator produces a container of that
		// name, so a foreign workload here reads as the platform — and one tagged with
		// the baseline passes green.
		if !isAplCoreImage(c.Image) {
			return fail("the %s/%s container %q runs %q, which is not an %s image — this is not apl-core's operator, so the "+
				"deployed apl-core version is UNKNOWN.%s",
				aplOperatorNamespace, deploy, containerName, c.Image, aplCoreImageName, unreadableRemedy())
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
// FAILS CLOSED ON EVERY FORM OF "COULD NOT TELL": a lane that reports success
// having read nothing looks exactly like the drift it exists to catch.
// owned says whether llz drives apl-core's version on this deployment
// (spec.cluster.bootstrap.manageAplVersion, which defaults ON). It changes what a
// minor/patch gap MEANS, which is why it is a parameter rather than something the
// grading infers: with Linode driving, a gap is the routine mid-rollout state and
// waiting is correct; with llz driving, the version was asserted into the values
// and a gap means the assertion did not take.
func evaluateAplDeployed(raw []byte, readErr error, owned bool) AplDeployedVerdict {
	if readErr != nil {
		return AplDeployedVerdict{Source: imageTagSource, Err: fmt.Errorf(
			"could not read the apl-operator Deployment in namespace %s, so the deployed apl-core version is UNKNOWN — "+
				"that is a failure, not a pass: %w", aplOperatorNamespace, readErr)}
	}
	var list deployList
	if err := json.Unmarshal(raw, &list); err != nil {
		return AplDeployedVerdict{Source: imageTagSource, Err: fmt.Errorf(
			"the apl-operator Deployment listing in namespace %s did not parse as JSON, so the deployed apl-core version is UNKNOWN: %w",
			aplOperatorNamespace, err)}
	}
	if len(list.Items) == 0 {
		return AplDeployedVerdict{Source: imageTagSource, Err: fmt.Errorf(
			"no Deployment at all in namespace %s — apl-core's operator is where the deployed platform version is legible, "+
				"so either the managed App Platform is not installed on this cluster or it has moved. Nothing was checked",
			aplOperatorNamespace)}
	}

	// SELECTED BY app.kubernetes.io/name — a DIFFERENT field from the one being read.
	// Selecting on the image would derive the expected set from the thing under test,
	// the shape where a filtered query returns nothing and the gate passes on the bug
	// it exists to catch.
	//
	// LABEL BEATS NAME: the label is a chart literal, while the Deployment name is
	// fullname-derived, so a stale Deployment can outlive a rename and answer first.
	// A candidate that cannot answer does not end the scan — its failure is held and
	// used only if nothing better turns up.
	var seen []string
	var byLabel, byName []deployItem
	for _, it := range list.Items {
		seen = append(seen, it.Metadata.Name)
		switch {
		case it.Metadata.Labels[nameLabel] == aplOperatorName:
			byLabel = append(byLabel, it)
		case it.Metadata.Name == aplOperatorName:
			byName = append(byName, it)
		}
	}
	candidates := append(append([]deployItem{}, byLabel...), byName...)

	var held *AplDeployedVerdict
	// EVERY "cannot answer" GOES THROUGH hold, so the invariant above covers all four
	// arms. Two of them returned outright, so a stale first candidate on a branch
	// build, or one with an unset otomi.version, failed the lane while the healthy
	// operator sat later in the same list.
	hold := func(v AplDeployedVerdict) {
		if held == nil {
			h := v
			held = &h
		}
	}
	for _, it := range candidates {
		image, bad := operatorImage(it.Metadata.Name, it.Spec.Template.Spec.Containers)
		if bad != nil {
			hold(*bad)
			continue
		}

		tag := imageTag(image)
		if tag == "" {
			hold(AplDeployedVerdict{Source: imageTagSource, Err: fmt.Errorf(
				"the %s/%s operator image %q carries no usable tag (%s), so the deployed apl-core version is UNKNOWN.%s",
				aplOperatorNamespace, it.Metadata.Name, image, tagFailure(image), unreadableRemedy())})
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
		// Whether Linode's managed installer ALWAYS pins a release is not something
		// llz verifies, and this arm has no per-instance override — if managed ever
		// ships "latest" this goes red fleet-wide.
		if _, _, _, ok := clusterspec.AplSemver(tag); !ok {
			hold(AplDeployedVerdict{Live: tag, Source: imageTagSource, Err: fmt.Errorf(
				"%s/%s runs operator image %q, whose tag %q is not a version this llz can compare against %s — apl-core allows a "+
					"branch name in otomi.version, so this is most likely a floating (non-release) platform install. The deployed "+
					"apl-core version is UNKNOWN.%s",
				aplOperatorNamespace, it.Metadata.Name, image, tag, clusterspec.BaselineAplChartVersion, unreadableRemedy())})
			continue
		}
		if implausibleMajor(tag) {
			hold(AplDeployedVerdict{Live: tag, Source: imageTagSource, Err: fmt.Errorf(
				"%s/%s runs operator image %q, whose tag %q predates every apl-core release (the oldest major published is %d), "+
					"so it is not a platform version at all rather than an old one. The usual cause is otomi.version being unset, "+
					"which makes the chart fall back to the apl-operator sub-chart's own appVersion; a foreign container in this "+
					"Deployment reads the same way. The deployed apl-core version is UNKNOWN.%s",
				aplOperatorNamespace, it.Metadata.Name, image, tag, aplCoreLowestPublishedMajor, unreadableRemedy())})
			continue
		}
		return classifyAplDeployed(tag, owned)
	}
	if held != nil {
		return *held
	}

	// NAME WHAT IS PRESENT. The thing being looked for may have been RENAMED, and no
	// amount of staring at the absent name reveals the new one.
	//
	// THE BLAST RADIUS OF THIS ARM IS EVERY ADOPTER AT ONCE, because the lane gates
	// the delivered scheduled health check — so the remedy has to be in the message.
	sort.Strings(seen)
	return AplDeployedVerdict{Source: imageTagSource, Err: fmt.Errorf(
		"no Deployment named %[1]q (or labelled %[2]s=%[1]s) in namespace %[3]s, "+
			"so the deployed apl-core version is UNKNOWN. Deployments present: %[4]s.%[5]s",
		aplOperatorName, nameLabel, aplOperatorNamespace, strings.Join(seen, ", "), unreadableRemedy())}
}

// classifyAplDeployed applies the SPEC-SIDE drift policy to the live version.
//
// THE BLOCK/ALLOW DECISION IS clusterspec.AplChartDriftBlocks, the shared
// predicate, not a threshold restated here: "how far apart" and "does that block"
// are different questions, and the escape hatch lives in the second. On managed App
// Platform losing it is worst of all, because LINODE moves the version — a lane
// that consulted only the distance reddens `assert-suite` and the scheduled health
// check on a condition the operator can neither fix nor opt out of.
func classifyAplDeployed(live string, owned bool) AplDeployedVerdict {
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

	// A STAGED MAJOR IS NOT A PATCH LAG, though both reach here: the shared predicate
	// permits a minor/patch gap outright, and a MAJOR one once AllowMajorDriftEnv is
	// set. One sentence for both makes a deliberately staged 7.0.0 read in the weekly
	// check exactly like a point-release lag. The override suppresses the block, not
	// the distance.
	if drift == clusterspec.AplChartDriftMajorBehind || drift == clusterspec.AplChartDriftMajorAhead {
		v.Warn = fmt.Sprintf(
			"this cluster runs apl-core %s, a MAJOR apart from the %s this llz release targets. It is not failing because %s is "+
				"set, which is a deliberate, time-boxed staging switch — llz has NOT been tested against this platform, so unset it "+
				"once the staged move is done rather than leaving it on",
			live, clusterspec.BaselineAplChartVersion, clusterspec.AllowMajorDriftEnv)
		return v
	}
	// THE SAME DISTANCE, TWO DIFFERENT FACTS, and which one it is depends on who
	// drives. Under Linode's rollout a gap is a schedule nobody here controls, so
	// warning and waiting is the honest answer. Once llz owns the version, that gap
	// is the mechanism reporting on itself: `llz render` wrote the version into
	// env/settings/otomi.yaml, the reconciler merged it onto the machine branch, and
	// apl-core was supposed to reconcile its operator to it. Still short means one of
	// those links did not hold — the chart-written apl-values Secret re-asserting its
	// own otomi.version, or the platform reverting the operator image — and a warning
	// on the very signal that proves the feature works is how a broken mechanism
	// stays green for a release cycle.
	if owned {
		v.Err = fmt.Errorf(
			"this cluster runs apl-core %s but this instance ASSERTS %s (spec.cluster.bootstrap.manageAplVersion "+
				"is on, so llz owns the version and wrote it to env/settings/otomi.yaml). The gap means the "+
				"assertion did not reach the operator: check that the apl-<env> branch carries the version "+
				"llz rendered, and that the chart-written apl-values Secret in %s is not re-asserting a "+
				"different otomi.version. Set manageAplVersion: false to hand the version back to Linode, "+
				"which makes this a warning again",
			live, clusterspec.BaselineAplChartVersion, aplOperatorNamespace)
		return v
	}
	v.Warn = fmt.Sprintf(
		"this cluster runs apl-core %s and this llz release targets %s. This deployment sets "+
			"manageAplVersion: false, so Linode owns the rollout (the API has no version field) and this is "+
			"the routine mid-rollout state — but it is the version llz was NOT tested against, and it is "+
			"what to check first if something behaves oddly",
		live, clusterspec.BaselineAplChartVersion)
	return v
}

// assertAplDeployedVersion is the lane.
func assertAplDeployedVersion() error {
	raw, err := deps.Exec("kubectl", "-n", aplOperatorNamespace, "get", "deploy", "-o", "json")
	v := evaluateAplDeployed(raw, err, aplVersionOwnedHere())
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

// aplVersionOwnedHere resolves whether llz drives the version for the deployment
// this lane is running against.
//
// FAIL-OPEN TO "OWNED", which is the direction that keeps the gate honest: the
// field defaults on, so an unreadable spec must not quietly downgrade a failure
// into a warning. The cost of being wrong here is a red lane on a cluster Linode
// actually drives, which an operator resolves by writing the field explicitly —
// far cheaper than a silent green on a mechanism that stopped working.
func aplVersionOwnedHere() bool {
	env := strings.TrimSpace(os.Getenv("REGION"))
	if env == "" {
		return true
	}
	lz, err := clusterspec.LoadSplit(".")
	if err != nil {
		return true
	}
	e, ok := lz.Env(env)
	if !ok {
		return true
	}
	return e.Cluster.Bootstrap.AplVersionManaged()
}
