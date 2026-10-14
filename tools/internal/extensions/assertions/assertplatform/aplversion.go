package assertplatform

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// clusterspec.MinSupportedAplChartVersion is the oldest apl-core chart the landing zone still
// supports. It used to be an ALIAS of clusterspec.BaselineAplChartVersion, with a note saying
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

func assertAplVersion(env string) error {
	v, err := clusterspec.ResolveAplChartVersion(env)
	if err != nil {
		return err
	}
	if err := clusterspec.AplVersionSupported(v, env); err != nil {
		return err
	}
	fmt.Printf("apl-core chart version %s (deployment %q) is supported (>= %s).\n", v, env, clusterspec.MinSupportedAplChartVersion)
	return nil
}
