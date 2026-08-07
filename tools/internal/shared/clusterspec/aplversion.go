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
// `apl-6.0.0` to `apl-v6.1.0`. `helm --version 6.1.0` still RESOLVES (helm treats
// the flag as a semver constraint and v6.1.0 normalises to 6.1.0), but only with
// an "unable to find exact version requested" warning, so we carry the exact
// published string. Every comparison here goes through AplSemver, which strips
// the prefix — a spec pinned to a bare "6.1.0" is still DriftNone.
const BaselineAplChartVersion = "v6.1.0"

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
// semver parser in package main (selfupdate.go) and they are deliberately NOT
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
	if drift != AplChartDriftMajorBehind && drift != AplChartDriftMajorAhead {
		return nil
	}
	if majorDriftAllowed() {
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
