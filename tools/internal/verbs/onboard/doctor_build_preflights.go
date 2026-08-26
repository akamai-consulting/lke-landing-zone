package onboard

// doctor_build_preflights.go — make `llz doctor` answer the question it claims
// to: "am I ready to build?"
//
// The apply path front-loads six cheap checks into its first job so a bad
// instance fails in seconds rather than after a ~15-minute cluster apply. Doctor
// covered three of them (the committed-render check, secret presence, and
// credential validity) and missed the rest:
//
//	llz ci assert-image-fresh    TF_IMAGE / KUBE_IMAGE vs the template pin
//	llz ci assert-apl-version    the apl-core chart floor
//	llz ci assert-k8s-version    the account can build the pinned k8sVersion
//	llz env pipeline --check     promote.yml names only declared deployments
//
// The fourth arrived the same way the first three did — as a CI job doctor had no
// counterpart for — and cost an adopter a red upgrade PR on a promote.yml that had
// been unrunnable since scaffold. See checkPromotionPipeline.
//
// Doctor-green then build-red is the worst outcome for an operator following the
// steps, because it invalidates the one signal they were told to trust. The first
// two checks are local, free and spec-only, so missing them had no upside: doctor
// went green, `llz up` dispatched, and CI rejected the instance on evidence doctor
// already had. (`llz up` covered the image half by accident — its `tokens` stage
// re-pins — but `llz up --skip-tokens` is documented in the quickstart, and a plain
// `doctor` → `build` misses it too.)
//
// THE THIRD IS COVERED DIFFERENTLY, AND DELIBERATELY. The chart floor and the
// image pins are answerable offline from the instance's own files, so doctor can
// reach CI's verdict and fold it into its exit status. The k8sVersion question
// cannot be: it needs a LINODE_TOKEN, and the account behind the operator's token
// need not be the account CI builds under (`llz tokens` PROMPTS for the PAT it
// pushes). A check that reads a different system than CI will read may report but
// must not decide — objlabel_preflight.go below is the same shape for the same
// reason. So doctor/linode.go reports it at full volume instead, naming
// `llz ci assert-k8s-version` as what will fail the build.
//
// THE CHECKS LIVE IN DIFFERENT PLACES, and that is not arbitrary — each runs
// where its evidence is. The chart floor is answerable from the spec alone, so it
// runs in doctor's local section and works offline. The image pins are a
// REPO-level fact: CI reads vars.TF_IMAGE, not a local file, so checking only
// `.llz/vars.env` would report ✓ for a fresh clone that has no `.llz/` at all — a
// false affirmative in exactly the state an upgrade produces. It therefore runs
// inside the e2e-readiness section, against the same merged local+live lookup
// `llz tokens` re-pins from. The k8sVersion pin is an ACCOUNT-level fact, so it
// runs where the Linode client already is (doctor/linode.go); putting a copy here
// would be a second answer to one question.
//
// NOT a re-implementation: the image check reuses staleCIImageVars (the same
// predicate `llz tokens` re-pins from) and the chart check reuses
// assertAplVersion's two halves, so neither can drift from what CI runs.

import (
	"fmt"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/promote"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
)

// checkAplChartFloor is assertAplVersion without its success print — the same
// resolve-then-test pair the CI verb runs, so doctor and the build cannot reach
// different verdicts about the same spec.
func checkAplChartFloor(env string) error {
	v, err := clusterspec.ResolveAplChartVersion(env)
	if err != nil {
		return err
	}
	return clusterspec.AplVersionSupported(v, env)
}

// checkPromotionPipeline is `llz env pipeline --check` run in-process — the same
// plan, the same verdict, the same remediation text the CI job prints.
//
// WHY DOCTOR HAS TO ASK THIS. The check existed only as a CI job, and
// promote.PlanWorkflow had exactly one caller in the tree: the `llz env pipeline`
// command. So the first thing that ever asked whether an instance's promote.yml
// could run was a job on a pull request — which is how gsap-apl carried a live
// `dev → staging → prod` pipeline over a spec declaring only `prod` from scaffold
// until an upgrade PR went red on it, with `llz doctor` (and `llz upgrade`, which
// runs doctor as its post-upgrade readiness report) green the whole way.
//
// IT IS DELIBERATELY FATAL, unlike the two advisories around it. Those describe
// live systems doctor can only see a corner of; this one is a comparison between
// two files in the tree doctor is standing in, and CI will reach the identical
// verdict on the identical bytes. Reporting it without failing would reproduce
// the doctor-green/build-red pattern this file exists to eliminate — the same
// argument that made the chart floor fatal.
//
// NOT --require-pipeline. "No pipeline yet" is the state every fresh instance is
// in and is not a readiness gap; only promote.yml's own preflight, which runs
// with the whole chain behind it, may treat it as one.
//
// A tree with no promote.yml at all, and the template-repo checkout, both return
// an empty Path — CheckReport's first arm — so this stays silent rather than
// asserting something about a file that is not there.
func checkPromotionPipeline() (lines []string, err error) {
	tfDir, _, relPrefix := instancelayout.Detect()
	plan, err := promote.PlanWorkflow(promote.DefaultDeps(), tfDir, relPrefix)
	if err != nil {
		return nil, err
	}
	return plan.CheckReport(true, false)
}

// checkSpecPreflights reports the CI preflights answerable from the local spec.
// Returns the errors to fold into doctor's exit status; prints its own section.
//
// Its caller gates on clusterspec.InstancePresent, which is the right guard for
// every arm here INCLUDING the promotion check: the expected side of that
// comparison is the declared deployment set, and the spec is where it comes from.
// (promote.DeploymentNames falls back to cluster tfvars when no spec exists, so a
// pre-spec instance is the one shape doctor would not ask about — CI still would.)
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

	// ── the promotion pipeline ────────────────────────────────────────────────
	// Deployment-INDEPENDENT: promote.yml is one file describing the whole
	// instance, so this runs even without an --env, and running it under a loop
	// over deployments would ask one question N times.
	if lines, perr := checkPromotionPipeline(); perr != nil {
		report("promotion pipeline (promote.yml)", false)
		// Indented under the ✗ like every other finding here. The message is
		// multi-line by design — it names each bad stage, the deployments that DO
		// exist, and the order to run the fix in — and flattening it would drop the
		// half that says which remedy applies.
		for _, l := range strings.Split(perr.Error(), "\n") {
			fmt.Printf("     %s\n", l)
		}
		errs = append(errs, perr)
	} else {
		report("promotion pipeline (promote.yml)", true)
		// Advisories: things `--check` could NOT verify. Printing them under a green
		// tick is the point — a ✓ that silently covered an unresolvable `region:`
		// would claim a comparison that never ran.
		for _, l := range lines {
			if !strings.HasPrefix(l, "promote.yml is in sync") {
				fmt.Printf("     %s\n", color.Dim(l))
			}
		}
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

func checkCIImagePins(tokensCmd string, recorded func(string) string) error {
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
	fmt.Printf("     re-pin with %s\n", color.Cyan(tokensCmd))
	return fmt.Errorf("%d ci image variable(s) name an older template commit than this instance's pin — "+
		"the first pipeline run fails `llz ci assert-image-fresh`", len(skew))
}
