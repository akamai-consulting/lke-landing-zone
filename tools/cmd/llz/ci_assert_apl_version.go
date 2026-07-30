package main

// ci_assert_apl_version.go implements `llz ci assert-apl-version` — a front-loaded
// preflight that refuses to stand up a cluster whose PINNED apl-core chart version
// the landing zone no longer supports.
//
// WHY THIS EXISTS. The v6 migration made the template apl-core-6.x-ONLY in two ways
// that are silent until the cluster is already running:
//
//   - it deleted the `apl-sops-secrets` empty-Secret placeholder, safe ONLY because
//     v6 made the operator's envFrom `optional: true`. On 5.x that envFrom is
//     `optional: false`, so `terraform apply` removing the placeholder leaves
//     apl-operator in CreateContainerConfigError ("secret \"apl-sops-secrets\" not
//     found") — the entire otomi control loop stops; and
//   - it retired the self-managed ESO because v6 bundles one. 5.x bundles NONE, so
//     the cluster ends up with no external-secrets at all: the CRDs never install
//     and every ESO-backed Secret (loki-object-store, harbor-registry-s3, …) never
//     materializes, taking loki and the harbor registry down with it.
//
// Both are knowable from the spec before any infrastructure exists, but they surface
// ~2h into a bootstrap as a pile of CreateContainerConfigError pods and a converge
// that burns its whole budget (observed on a live prod bootstrap). Fail in seconds,
// naming the fix, instead.
//
// This gate is the FLOOR only. Drift from the baseline this release targets — an
// instance still pinned to 6.0.0 while the baseline is v6.1.0 — is a separate,
// non-blocking signal (clusterspec.AplChartVersionWarnings).

import (
	"fmt"

	"github.com/spf13/cobra"
)

// minSupportedAplChartVersion is the oldest apl-core chart the landing zone still
// supports. It used to be an ALIAS of defaultAplChartVersion, with a note saying
// to give it its own literal on the day the floor and the target legitimately
// diverged. The v6.1.0 baseline bump is that day.
//
// The floor stays 6.0.0 because nothing in the 6.1.0 upgrade made the landing
// zone 6.1-only: the apl-values env-tree paths (env/apps/<name>.yaml,
// env/settings/obj.yaml, env/teams/<name>/), the apl-secrets/apl-git-config BYO-Git
// Secret contract, and the external-secrets.io/v1 + v1alpha1 PushSecret API
// versions LLZ writes are all unchanged between 6.0.0 and 6.1.0. Raising the floor
// in lockstep would have hard-failed every instance still on 6.0.0 for no reason —
// minor drift is a WARNING (clusterspec.AplChartVersionWarnings), not a block.
//
// Raise it only when a 6.0.0 cluster genuinely stops working, and say why here the
// way the 5.x rationale below does.
const minSupportedAplChartVersion = "6.0.0"

func ciAssertAplVersionCmd() *cobra.Command {
	var env string
	c := &cobra.Command{
		Use:   "assert-apl-version",
		Short: "fail fast when the spec pins an apl-core chart version the landing zone no longer supports",
		Long: "Resolves the apl-core chart version exactly as `llz ci bootstrap-cluster` does\n" +
			"(spec.cluster.bootstrap.aplChartVersion for the deployment, else the baked\n" +
			"default) and fails when it is older than " + minSupportedAplChartVersion + ".\n\n" +
			"Run as a front-loaded preflight so an unsupported pin fails in seconds rather\n" +
			"than wedging apl-operator (missing apl-sops-secrets) and leaving the cluster\n" +
			"with no external-secrets operator — both ~2h into the bootstrap.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return assertAplVersion(env) },
	}
	c.Flags().StringVar(&env, "env", "", "deployment whose spec pin to check (e.g. prod); empty checks the baked default only")
	return c
}

// resolveAplChartVersion mirrors runBootstrapCluster's resolution: the deployment's
// spec pin when present, else the baked default. A missing spec/deployment is not an
// error here — it simply means the default applies.
func resolveAplChartVersion(env string) (string, error) {
	pinned := ""
	lz, present, err := loadSpec()
	if err != nil {
		return "", fmt.Errorf("load spec to resolve the apl-core chart version: %w", err)
	}
	if present && env != "" {
		if e, ok := lz.Env(env); ok {
			pinned = e.Cluster.Bootstrap.AplChartVersion
		}
	}
	return firstNonEmpty(pinned, defaultAplChartVersion), nil
}

func assertAplVersion(env string) error {
	v, err := resolveAplChartVersion(env)
	if err != nil {
		return err
	}
	if err := aplVersionSupported(v, env); err != nil {
		return err
	}
	fmt.Printf("apl-core chart version %s (deployment %q) is supported (>= %s).\n", v, env, minSupportedAplChartVersion)
	return nil
}

// aplVersionSupported is the pure predicate behind the preflight: nil when v is a
// semver >= minSupportedAplChartVersion, else an error explaining exactly what
// breaks and how to fix it. Split out from assertAplVersion so it is testable
// without a spec on disk.
func aplVersionSupported(v, env string) error {
	if _, _, _, ok := semver(v); !ok {
		return fmt.Errorf("apl-core chart version %q (deployment %q) is not a semver — set spec.cluster.bootstrap.aplChartVersion to a released apl-core chart (>= %s)",
			v, env, minSupportedAplChartVersion)
	}
	if semverLess(v, minSupportedAplChartVersion) {
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
			v, env, minSupportedAplChartVersion,
			v, v, minSupportedAplChartVersion, env, v, importInitAplChartVersion)
	}
	return nil
}
