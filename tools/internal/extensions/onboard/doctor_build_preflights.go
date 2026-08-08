package onboard

// doctor_build_preflights.go — make `llz doctor` answer the question it claims
// to: "am I ready to build?"
//
// The apply path front-loads five cheap checks into its first job so a bad
// instance fails in seconds rather than after a ~15-minute cluster apply. Doctor
// covered three of them (the committed-render check, secret presence, and
// credential validity) and missed two:
//
//	llz ci assert-image-fresh    TF_IMAGE / KUBE_IMAGE vs the template pin
//	llz ci assert-apl-version    the apl-core chart floor
//
// Both are local, free, and spec-only, so the miss had no upside: doctor went
// green, `llz up` dispatched, and CI rejected the instance on evidence doctor
// already had. `llz up` covered the image half by accident — its `tokens` stage
// re-pins — but `llz up --skip-tokens` is documented in the quickstart, and a
// plain `doctor` → `build` misses it too. The chart floor was missed on every
// path, and its own workflow comment says the failure it prevents otherwise
// surfaces "~2h in as CreateContainerConfigError pods".
//
// Doctor-green then build-red is the worst outcome for an operator following the
// steps, because it invalidates the one signal they were told to trust.
//
// THE TWO CHECKS LIVE IN DIFFERENT PLACES, and that is not arbitrary. The chart
// floor is answerable from the spec alone, so it runs in doctor's local section
// and works offline. The image pins are a REPO-level fact: CI reads
// vars.TF_IMAGE, not a local file, so checking only `.llz/vars.env` would report
// ✓ for a fresh clone that has no `.llz/` at all — a false affirmative in exactly
// the state an upgrade produces. It therefore runs inside the e2e-readiness
// section, against the same merged local+live lookup `llz tokens` re-pins from.
//
// NOT a re-implementation: the image check reuses staleCIImageVars (the same
// predicate `llz tokens` re-pins from) and the chart check reuses
// assertAplVersion's two halves, so neither can drift from what CI runs.

import (
	"fmt"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertplatform"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// checkAplChartFloor is assertAplVersion without its success print — the same
// resolve-then-test pair the CI verb runs, so doctor and the build cannot reach
// different verdicts about the same spec.
func checkAplChartFloor(env string) error {
	v, err := assertplatform.ResolveAplChartVersion(env)
	if err != nil {
		return err
	}
	return assertplatform.AplVersionSupported(v, env)
}

// checkSpecPreflights reports the CI preflights answerable from the local spec.
// Returns the errors to fold into doctor's exit status; prints its own section.
func checkSpecPreflights(env string) []error {
	fmt.Println("\n" + color.Bold("Build preflights (what CI checks before the apply):"))
	var errs []error

	// ── the apl-core chart floor ──────────────────────────────────────────────
	// Deployment-scoped, so it only runs with an env to check.
	if env == "" {
		fmt.Println(color.Dim("  (no deployment selected — pass --env <env> for the chart-version check)"))
	} else if err := checkAplChartFloor(env); err != nil {
		report("apl-core chart version", false)
		fmt.Printf("     %s\n", err)
		errs = append(errs, err)
	} else {
		report("apl-core chart version", true)
	}

	// ── object-storage bucket labels ──────────────────────────────────────────
	// Advisory (objlabel_preflight.go): it can only see this account's buckets, so
	// it never gates. Naming the labels here is what gives the operator the right
	// vocabulary if the apply does collide.
	if env != "" {
		if prefix, perr := clusterspec.LabelPrefixFor("`llz doctor`"); perr == nil {
			checkBucketLabelsAvailable(prefix, env)
		}
	}
	return errs
}

func checkCIImagePins(recorded func(string) string) error {
	ref := answers.PinnedTemplateRef()
	if ref == "" {
		return nil // not a pinned instance — nothing to compare against
	}
	anyRecorded := false
	for _, name := range templatecommit.CIImageVarNames() {
		if strings.TrimSpace(recorded(name)) != "" {
			anyRecorded = true
			break
		}
	}
	if !anyRecorded {
		fmt.Printf("  %s  %s\n", color.Dim("—"),
			color.Dim("TF_IMAGE / KUBE_IMAGE not set yet — `llz tokens` computes them (reported above)"))
		return nil
	}
	skew := templatecommit.StaleCIImageVars(ref, recorded)
	if len(skew) == 0 {
		report("TF_IMAGE / KUBE_IMAGE match the template pin", true)
		return nil
	}
	report("TF_IMAGE / KUBE_IMAGE match the template pin", false)
	for _, s := range skew {
		fmt.Printf("     %s\n       have %s\n       want %s\n", color.Bold(s.Name), color.Dim(s.Have), color.Cyan(s.Want))
	}
	// Same remediation `llz ci assert-image-fresh` prints, verbatim in spirit: an
	// operator who ignores this meets that one next, and the two must read as one
	// instruction rather than two problems.
	fmt.Printf("     re-pin with %s\n", color.Cyan("llz tokens --env <env> --yes"))
	return fmt.Errorf("%d ci image variable(s) name an older template commit than this instance's pin — "+
		"the first pipeline run fails `llz ci assert-image-fresh`", len(skew))
}
