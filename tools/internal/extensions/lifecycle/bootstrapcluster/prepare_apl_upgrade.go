package bootstrapcluster

// ci_prepare_apl_upgrade.go implements `llz ci prepare-apl-upgrade` — the one
// cluster-side action apl-core's 6.1.0 release notes list as a HARD prerequisite
// that must happen BEFORE the upgrade rolls:
//
//	kubectl annotate deployment apl-operator \
//	  argocd.argoproj.io/sync-options=Force=true,Replace=true -n apl-operator
//
// WHY IT CANNOT WAIT FOR THE UPGRADE. 6.1.0 adds the annotation to the operator's
// own chart (values/apl-operator/apl-operator.gotmpl gained `deploymentAnnotations:
// argocd.argoproj.io/sync-options: Force=true,Replace=true`). But that value only
// lands once the 6.1.0 chart has been applied — and applying it is the very sync
// that needs the annotation. Without it Argo CD attempts a normal apply over the
// 6.0.0 Deployment, whose immutable/conflicting fields the 6.1.0 spec rewrites, and
// the sync fails with the operator left mid-upgrade. Annotating first makes that
// sync a Replace.
//
// WHY LLZ RUNS IT AT ALL. On the managed App Platform Linode owns the apl-core
// version and picks the moment it rolls (ADR 0005) — LLZ never runs `helm upgrade`
// on apl-core. That means LLZ has no upgrade hook to hang this on, and an operator
// cannot reliably win a race against a managed rollout by hand. So it is applied
// EAGERLY, on every bootstrap: idempotent, cheap, and harmless once 6.1.0's own
// chart sets the same value (the annotation is then simply re-asserted, and this
// step self-retires when the 6.0.x fleet is gone).
//
// It is deliberately best-effort at the call site in bootstrapCluster: a cluster
// that has no apl-operator Deployment yet (a fresh managed install still coming
// up) is not a bootstrap failure, it just means there is nothing to prepare.

import (
	"fmt"
	"os"
	"strings"
)

// The apl-operator Deployment coordinates + the annotation upstream requires.
// Split out as constants so the test asserts the exact upstream contract rather
// than a re-spelling of it.
const (
	AplOperatorDeployment = "apl-operator"
	AplOperatorNamespace  = "apl-operator"
	AplSyncOptionsKey     = "argocd.argoproj.io/sync-options"
	AplSyncOptionsValue   = "Force=true,Replace=true"
)

// PrepareAplUpgrade annotates the apl-operator Deployment with the sync-options
// apl-core 6.1.0 requires before its upgrade syncs.
//
// applied=false, err=nil is the ABSENT case: no apl-operator Deployment (a cluster
// whose managed apl-core has not been installed yet). That is a legitimate state,
// not a failure — distinguishing it from a real annotate failure is the whole
// reason this returns a bool rather than swallowing both into nil.
//
// --overwrite is required: without it kubectl refuses when the key already exists,
// so the second bootstrap of any cluster would error on an unchanged annotation.
func PrepareAplUpgrade(d bootstrapDeps) (applied bool, err error) {
	if _, ok := d.kubectl("-n", AplOperatorNamespace, "get", "deployment", AplOperatorDeployment,
		"-o", "name"); !ok {
		return false, nil
	}
	out, ok := d.kubectl("-n", AplOperatorNamespace, "annotate", "--overwrite",
		"deployment", AplOperatorDeployment, AplSyncOptionsKey+"="+AplSyncOptionsValue)
	if !ok {
		return false, fmt.Errorf("annotate %s/%s with %s: %s",
			AplOperatorNamespace, AplOperatorDeployment, AplSyncOptionsKey, strings.TrimSpace(out))
	}
	return true, nil
}

// PrepareAplUpgradeBestEffort is the bootstrap-cluster call site: it warns instead
// of aborting. Missing the annotation degrades a FUTURE managed upgrade; it does
// not affect the bridge this bootstrap is placing, so it must not fail the apply.
func PrepareAplUpgradeBestEffort(d bootstrapDeps) {
	applied, err := PrepareAplUpgrade(d)
	switch {
	case err != nil:
		warn(fmt.Sprintf("could not annotate %s/%s with %s=%s (%v) — apl-core's 6.1.x upgrade may fail to sync until it is applied by hand (llz ci prepare-apl-upgrade).",
			AplOperatorNamespace, AplOperatorDeployment, AplSyncOptionsKey, AplSyncOptionsValue, err))
	case applied:
		fmt.Fprintf(os.Stderr, "→ %s/%s annotated %s=%s (apl-core 6.1.x upgrade prerequisite).\n",
			AplOperatorNamespace, AplOperatorDeployment, AplSyncOptionsKey, AplSyncOptionsValue)
	}
}

// warn prints a GitHub Actions warning annotation.
//
// A COPY, NOT AN EXPORT. It was defined in ci_kyverno.go, which moved to
// tools/internal/extensions/lifecycle/kyverno; two package main files use it, and exporting a two-line
// printf so both sides can reach it would put a symbol in a package's API whose
// only job is to be reachable. Fixtures and printers travel by copy — the same
// call the campaign made for firstNonEmpty, orAll and report.
func warn(msg string) { fmt.Printf("::warning::%s\n", msg) }
