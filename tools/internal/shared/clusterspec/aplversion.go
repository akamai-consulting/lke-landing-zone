package clusterspec

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// BaselineAplChartVersion is the apl-core chart version THIS llz release targets.
// It is both the fallback an environment deploys when
// spec.cluster.bootstrap.aplChartVersion is omitted AND the version every
// explicit pin is checked against (see AplChartDriftOf).
//
// Bump it in lockstep with the platform's baseline apl-core upgrade. The drift
// check below then fails every instance still pinned to the previous major —
// exactly the signal that was missing when an APL 5 instance upgraded llz to the
// APL 6 release and its stale `aplChartVersion: 5.0.0` pin silently kept
// deploying APL 5.
//
// NOTE THE LEADING "v". Up to and including 6.0.0 apl-core published its Helm
// chart with a bare `version: 6.0.0`; the release-automation rework that shipped
// with 6.1.0 (upstream ADR 2026-06-02-release-branch-per-cycle) changed the
// published Chart.yaml to `version: v6.1.0`, and the chart git tag from
// `apl-6.0.0` to `apl-v6.1.0`. That convention has held since (verified against
// the published index for this pin: `version: v6.2.1`, tag `apl-v6.2.1`).
// `helm --version 6.2.1` still RESOLVES (helm treats the flag as a semver
// constraint and v6.2.1 normalises to 6.2.1), but only with an "unable to find
// exact version requested" warning, so we carry the exact published string.
// Every comparison here goes through AplSemver, which strips the prefix — a spec
// pinned to a bare "6.2.1" is still DriftNone.
//
// v6.2.1 is a PATCH over v6.2.0 and the first patch-level baseline this repo has
// carried, so note what that does to an instance still pinned to v6.2.0: patch
// drift classifies as AplChartDriftMinor, which WARNS and never blocks. Nothing
// in the release forces the move — it is three BYO-DNS render fixes
// (linode/apl-core#3549 external-dns + cert-manager settings, #3559 external-dns
// secrets) on the exact path LLZ drives with its Linode DNS token. apl-core's
// values-schema.yaml is the SAME BLOB at both tags (git sha 9502538c), so nothing
// downstream of this constant has to move with it.
const BaselineAplChartVersion = "v6.2.1"

// AplBaselineHistory is every apl-core chart version an llz release has targeted,
// oldest first, WITH THE CURRENT BASELINE AS THE LAST ELEMENT.
//
// It exists so `llz upgrade` can tell a pin that was TRACKING US from a pin the
// operator chose. `spec.cluster.bootstrap.aplChartVersion` is optional and an
// omitted pin resolves to the baseline — so an instance that writes the baseline
// into the field is stating what the default already says, and the statement goes
// stale the next time the baseline moves. A pin matching any entry here is one of
// ours; anything else is a deliberate choice and must survive an upgrade
// untouched. Without this list the upgrade would have to guess, and guessing wrong
// silently moves an environment the operator meant to hold back.
//
// THE LAST ELEMENT IS THE BASELINE, deliberately, so the two cannot drift: the
// constant below is asserted against `AplBaselineHistory[len-1]`, which makes it
// impossible to bump the baseline without recording the version it replaced.
// A plain "previous baselines" list would have rotted the first time someone
// bumped and forgot to append — and the failure would have been silent, because a
// missing entry only means an old pin stops being recognised as ours.
var AplBaselineHistory = []string{"6.0.0", "v6.1.0", "v6.2.0", "v6.2.1"}

// WasAplBaseline reports whether a pin names a version llz has ever targeted.
//
// Only the leading "v" is normalised away, so a bare "6.2.0" matches the published
// "v6.2.0" — an instance that dropped the prefix is still tracking us.
//
// A PRE-RELEASE IS NOT THE RELEASE, and that is why this does NOT go through
// AplSemver like every other comparison in this file. AplSemver deliberately
// strips `-rc.1` because the DRIFT question cares about the numeric triple and not
// the release channel. This question is the opposite: llz has never targeted
// `v6.2.1-rc.1`, so a pin naming it is an operator deliberately riding a release
// candidate — exactly the choice `llz upgrade` must leave alone. Reusing the
// lenient parser here would have deleted it as "one of ours".
func WasAplBaseline(pin string) bool {
	norm := func(v string) string {
		return strings.TrimPrefix(strings.TrimSpace(v), "v")
	}
	pin = norm(pin)
	if pin == "" {
		return false
	}
	for _, b := range AplBaselineHistory {
		if norm(b) == pin {
			return true
		}
	}
	return false
}

// AllowMajorDriftEnv opts an instance out of the major-version gate for the
// duration of a staged upgrade (e.g. dev pinned a major ahead of prod while the
// new release bakes). Set it on the workflow dispatch, not in the spec: it is a
// time-boxed operator override, not a property of the landing zone.
const AllowMajorDriftEnv = "LLZ_ALLOW_APL_CHART_MAJOR_DRIFT"

// EffectiveAplChartVersion resolves what an environment actually deploys: the
// explicit pin when set, else the baseline. Every consumer that needs the
// deployed version (bootstrap-cluster's `helm --version`, the apl-values schema
// gate) must resolve through this rather than reading the raw field, so an
// omitted pin never degrades to "no version" downstream.
func EffectiveAplChartVersion(pin string) string {
	if pin == "" {
		return BaselineAplChartVersion
	}
	return pin
}

// AplChartDrift classifies an explicit pin relative to BaselineAplChartVersion.
type AplChartDrift int

const (
	AplChartDriftNone AplChartDrift = iota
	AplChartDriftUnparseable
	AplChartDriftMajorBehind
	AplChartDriftMajorAhead
	AplChartDriftMinor
)

// AplChartDriftOf classifies a pin against the baseline. An empty pin is
// DriftNone — it resolves to the baseline by construction.
func AplChartDriftOf(pin string) AplChartDrift {
	if pin == "" {
		return AplChartDriftNone
	}
	pMaj, pMin, pPatch, ok := AplSemver(pin)
	if !ok {
		return AplChartDriftUnparseable
	}
	bMaj, bMin, bPatch, ok := AplSemver(BaselineAplChartVersion)
	if !ok {
		// An unparseable baseline is a build-time bug in llz itself; don't
		// convert it into a spec problem the operator cannot fix.
		return AplChartDriftNone
	}
	switch {
	case pMaj < bMaj:
		return AplChartDriftMajorBehind
	case pMaj > bMaj:
		return AplChartDriftMajorAhead
	case pMin != bMin || pPatch != bPatch:
		return AplChartDriftMinor
	}
	return AplChartDriftNone
}

// AplSemver parses a bare MAJOR.MINOR.PATCH chart version.
//
// EXPORTED for internal/assertplatform, whose apl-version lane compares a pinned
// chart version against the minimum this llz supports. Note there is a SECOND
// semver parser in internal/verbs/selfupgrade (selfupdate.go) and they are deliberately NOT
// merged: that one strips a leading "llz/" because it parses llz RELEASE TAGS,
// and this one TrimSpaces and rejects negatives because it parses operator-typed
// CHART versions out of a spec file. Same shape, different inputs — collapsing
// them would make each wrong for the other's source. A leading "v" and any
// pre-release/build suffix (6.1.0-rc.1) are tolerated and ignored — the gate
// cares about the numeric triple, not the release channel.
func AplSemver(s string) (maj, min, patch int, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, 0, 0, false
		}
		out[i] = n
	}
	return out[0], out[1], out[2], true
}

// majorDriftAllowed reports whether the operator has opted out of the
// major-version gate for this run.
func majorDriftAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(AllowMajorDriftEnv))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// AplChartDriftBlocks reports whether a drift classification is a BLOCKING one,
// honouring the operator's staged-upgrade override.
//
// EXPORTED, AND THE ONLY COPY OF THIS RULE — but read the next paragraph before
// relying on the count. The rule is "major in either direction, unless the escape
// hatch is set", and restating it anywhere else is how two gates come to disagree:
// the live lane was written without the escape-hatch arm and would have reddened a
// gating check on a condition its operator could neither fix nor override, because
// on managed App Platform LINODE moves the version.
//
// ONE PRODUCTION CONSUMER, NOT TWO. assertplatform's assert-apl-deployed-version
// decides through this. aplChartVersionError below — the spec-side gate this was
// extracted to share WITH — turns out to have no production caller at all: it and
// AplChartVersionWarnings are reachable only from their own tests. The delivered
// preflight is `llz ci assert-apl-version`, which calls AplVersionSupported, a plain
// >= MinSupportedAplChartVersion floor check that knows nothing about drift or the
// override.
//
// That is recorded rather than fixed here because wiring a dormant gate would start
// failing instances that pass today — a decision with its own blast radius, not a
// side effect of a version bump. What it does change is what may be CLAIMED: a
// message telling an operator that a major-ahead pin "will BLOCK validation, set
// AllowMajorDriftEnv" was wrong twice over, and aplpin.go asks AplVersionSupported
// now precisely because that is the predicate the delivered gate runs.
func AplChartDriftBlocks(drift AplChartDrift) bool {
	if drift != AplChartDriftMajorBehind && drift != AplChartDriftMajorAhead {
		return false
	}
	return !majorDriftAllowed()
}

// aplChartVersionError returns the BLOCKING problem with an environment's pin,
// or nil. Major drift blocks in both directions: a pin a major behind the
// baseline is the silent-stale case (the instance keeps deploying the old APL
// straight through an llz upgrade), and a pin a major ahead is a version this
// llz release has not been tested against. Minor/patch drift only warns (see
// AplChartVersionWarnings) — it is routine mid point-release-rollout, and
// blocking it would wedge every instance between bumps.
func aplChartVersionError(env, pin string) error {
	drift := AplChartDriftOf(pin)
	if drift == AplChartDriftUnparseable {
		return fmt.Errorf("environments.%s.cluster.bootstrap.aplChartVersion %q is not a MAJOR.MINOR.PATCH chart version", env, pin)
	}
	if !AplChartDriftBlocks(drift) {
		return nil
	}
	pMaj, _, _, _ := AplSemver(pin)
	if drift == AplChartDriftMajorBehind {
		return fmt.Errorf(
			"environments.%s.cluster.bootstrap.aplChartVersion is %q but this llz release targets apl-core %s — "+
				"the pin overrides the baseline, so this environment would keep deploying APL %d across the upgrade. "+
				"Bump the pin to %s (see docs/apl-core-migration-runbook.md), or set %s=1 to stage the upgrade deliberately",
			env, pin, BaselineAplChartVersion, pMaj, BaselineAplChartVersion, AllowMajorDriftEnv)
	}
	return fmt.Errorf(
		"environments.%s.cluster.bootstrap.aplChartVersion is %q, a major ahead of the apl-core %s this llz release targets — "+
			"llz has not been tested against APL %d. Upgrade llz first, or set %s=1 to stage the upgrade deliberately",
		env, pin, BaselineAplChartVersion, pMaj, AllowMajorDriftEnv)
}

// AplChartVersionWarnings returns the NON-blocking apl-core version drift across
// every environment: pins on the baseline's major but off its minor/patch. These
// are reported next to the validation result rather than failing it, so a
// point-release lag is visible without wedging apply.
func (lz *LandingZone) AplChartVersionWarnings() []string {
	var out []string
	for _, name := range lz.EnvNames() {
		pin := lz.Spec.Environments[name].Cluster.Bootstrap.AplChartVersion
		if AplChartDriftOf(pin) != AplChartDriftMinor {
			continue
		}
		out = append(out, fmt.Sprintf(
			"environments.%s.cluster.bootstrap.aplChartVersion is %q; this llz release targets apl-core %s",
			name, pin, BaselineAplChartVersion))
	}
	return out
}

// AplSemverLess reports whether chart version a sorts before b. An unparseable
// version sorts lowest, so an unset or malformed pin reads as "older than the
// minimum" rather than silently passing a floor check.
func AplSemverLess(a, b string) bool {
	amaj, amin, apatch, aok := AplSemver(a)
	bmaj, bmin, bpatch, bok := AplSemver(b)
	if !aok || !bok {
		return !aok && bok
	}
	if amaj != bmaj {
		return amaj < bmaj
	}
	if amin != bmin {
		return amin < bmin
	}
	return apatch < bpatch
}

// ── SUPPORTED-VERSION PREDICATE AND RESOLUTION ───────────────────────────────
//
// These came from internal/extensions/assertplatform, where AplVersionSupported
// was already labelled "the pure predicate behind the preflight ... split out from
// assertAplVersion so it is testable without a spec on disk". It was split once
// for testability and needed splitting again for layering: `onboard` imported the
// whole platform-assertion capability to ask whether a version is supported, and
// that is a fact about the SPEC.
//
// The AplSemver / AplSemverLess / BaselineAplChartVersion they are built on were
// already in this file, so the predicate had been reaching back down here all
// along -- aplsemverless_test.go's header even says AplSemverLess "was added for
// internal/assertplatform's chart-floor check".

// Raise it only when a 6.0.0 cluster genuinely stops working, and say why here the
// way the 5.x rationale below does.
// MinSupportedAplChartVersion is EXPORTED because one assertion needs both it and
// the scaffold baseline: an instance `llz import` creates must be born on a chart
// this gate would not refuse. That test now lives here, since this is the side
// that owns the floor.
const MinSupportedAplChartVersion = "6.0.0"

// ResolveAplChartVersion mirrors runBootstrapCluster's resolution: the deployment's
// spec pin when present, else the baked default. A missing spec/deployment is not an
// error here — it simply means the default applies.
func ResolveAplChartVersion(env string) (string, error) {
	pinned := ""
	// Detected() DIRECTLY. This is the THIRD one-implementation LoadSpec seam this
	// sweep has collapsed -- envtopology's and configreadiness's were the others,
	// and all three were installed by package main as exactly `Detected()`
	// while the package default returned something inert.
	lz, present, err := Detected()
	if err != nil {
		return "", fmt.Errorf("load spec to resolve the apl-core chart version: %w", err)
	}
	if present && env != "" {
		if e, ok := lz.Env(env); ok {
			pinned = e.Cluster.Bootstrap.AplChartVersion
		}
	}
	return firstNonEmpty(pinned, BaselineAplChartVersion), nil
}

// AplVersionSupported is the pure predicate behind the preflight: nil when v is a
// semver >= MinSupportedAplChartVersion, else an error explaining exactly what
// breaks and how to fix it. Split out from assertAplVersion so it is testable
// without a spec on disk.
func AplVersionSupported(v, env string) error {
	if _, _, _, ok := AplSemver(v); !ok {
		return fmt.Errorf("apl-core chart version %q (deployment %q) is not a semver — set spec.cluster.bootstrap.aplChartVersion to a released apl-core chart (>= %s)",
			v, env, MinSupportedAplChartVersion)
	}
	if AplSemverLess(v, MinSupportedAplChartVersion) {
		//lint:ignore ST1005 multi-line operator diagnostic: the trailing period ends a sentence of remediation prose that continues on the next line
		return fmt.Errorf(`apl-core chart version %q (deployment %q) is NOT supported — this landing zone requires >= %s.

The v6 migration made the template apl-core-6.x-only in two ways that do not fail
until the cluster is already up, and then only as cryptic pod errors:

  * the `+"`apl-sops-secrets`"+` placeholder was dropped (v6 made the operator's envFrom
    optional:true). On %s it is optional:false, so apl-operator dies with
    CreateContainerConfigError "secret \"apl-sops-secrets\" not found" and the whole
    otomi control loop stops.
  * the self-managed external-secrets operator was retired (v6 bundles one). %s
    bundles none, so the cluster gets NO ESO: the CRDs never install and every
    ESO-backed Secret (loki-object-store, harbor-registry-s3, …) never materializes.

Fix one of:
  * set spec.cluster.bootstrap.aplChartVersion to >= %s for deployment %q (preferred), or
  * pin the instance to a template release that still supported %s.

NOTE: `+"`llz import init`"+` scaffolds apl-core %s (this release's baseline), which is
already above this floor — so a rejection here means an EXISTING pin was carried
across an llz upgrade, not that the scaffolder produced it.`,
			v, env, MinSupportedAplChartVersion,
			v, v, MinSupportedAplChartVersion, env, v, BaselineAplChartVersion)
	}
	return nil
}

// firstNonEmpty is a four-line local copy, as several packages in this tree keep.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
