package main

// ci_assert_image_fresh.go implements `llz ci assert-image-fresh` — a fast
// preflight that fails LOUD when the ci-tofu image's baked `llz` is not built
// from the template ref the instance pins.
//
// WHY: an instance pins TF_IMAGE (the container whose baked llz the jobs run)
// separately from its template pin (the TF roots + workflow source), so they
// drift. When the image lags, the checked-out workflow calls llz subcommands/
// flags the baked binary doesn't have — surfacing as a silent no-op readiness
// gate (the AppProject CRD race in PR #86) or a cryptic "unknown flag" ~20 min
// into a run. When it LEADS, its newer renderer disagrees with the committed
// manifests and `llz render --check` fails one step later. This guard turns
// both into a clear failure at the FIRST job.
//
// A TAG pin against a `dev-<sha>` image is NOT skipped: the tag is resolved to
// the commit it names and compared as commits (template_commit.go). That case
// was skipped until a live adopter hit it — and it is not exotic, it is what a
// freshly scaffolded instance looks like by default.
//
// The ref is read from the instance's own .copier-answers.yml rather than passed
// in as a workflow input: a hand-maintained input is a third pin that can skew
// from the other two, which is the very failure this guard exists to catch.

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// hexSHARe matches a git object name (short or full). Anchored so a branch/tag
// name like "main" or "v1.2.3" is NOT treated as a SHA.
var hexSHARe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func ciAssertImageFreshCmd() *cobra.Command {
	var templateRef string
	c := &cobra.Command{
		Use:   "assert-image-fresh",
		Short: "fail if the baked llz is older than the instance's pinned template ref (image/source skew guard)",
		Long: "Compares the ci-tofu image's baked llz build (main.version, stamped\n" +
			"`dev-<github.sha>` for dev images or a release tag) against the ref this\n" +
			"instance pins (.copier-answers.yml). A dev image's SHA must match the commit\n" +
			"the pin names — a TAG pin is resolved to its commit first, so tag-vs-SHA is\n" +
			"compared rather than skipped. A release image's tag must equal the pinned tag.\n" +
			"On mismatch it FAILS with an actionable message (the exact TF_IMAGE/KUBE_IMAGE\n" +
			"re-pin). When it cannot compare at all (unstamped local build, an unresolvable\n" +
			"ref, a release image against a SHA pin) it warns and passes — this never blocks\n" +
			"on evidence it does not have.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Check the pin against ITSELF before comparing it to the image. This
			// guard is the instance's only CI-run reader of .copier-answers.yml, and
			// `llz lint` does NOT run in an instance's CI (pre-commit + template CI
			// only) — so without this call the skew reaches a cluster whenever the
			// operator commits the upgrade with --no-verify. An explicit
			// --template-ref overrides the pin, so there is nothing to hold to it.
			if templateRef == "" {
				if err := assertPinCoherence("."); err != nil {
					return err
				}
			}
			return runAssertImageFresh(version, firstNonEmpty(templateRef, pinnedTemplateRef()))
		},
	}
	c.Flags().StringVar(&templateRef, "template-ref", "", "override the ref compared against the baked llz build (default: the instance's pin)")
	return c
}

func runAssertImageFresh(bakedVersion, templateRef string) error {
	templateRef = strings.TrimSpace(templateRef)
	if templateRef == "" {
		return fmt.Errorf("cannot resolve the template ref to compare against: no .copier-answers.yml in the working directory (run from an instance checkout, or pass --template-ref)")
	}
	baked := strings.TrimSpace(bakedVersion)
	if baked == "" || baked == "dev" {
		fmt.Fprintf(os.Stderr, "::warning::assert-image-fresh: baked llz version is unstamped (%q) — cannot verify image/template freshness; skipping.\n", bakedVersion)
		return nil
	}

	refIsSHA := hexSHARe.MatchString(templateRef)
	bakedSHA := ""
	if s := strings.TrimPrefix(baked, "dev-"); s != baked {
		bakedSHA = s
	}

	if bakedSHA != "" { // dev image — compare SHAs
		compareTo := templateRef
		if !refIsSHA {
			// A TAG pin against a dev-SHA image used to warn and skip. That skip
			// covered the DEFAULT new-adopter shape — copier pins a release tag while
			// TF_IMAGE floats on a tag main republishes — so the guard passed on
			// precisely the instances it exists to protect. Resolve the tag to the
			// commit it names and compare the two as commits. See template_commit.go.
			sha, ok := resolveTemplateCommit(instanceTemplateRepo(), templateRef)
			if !ok {
				fmt.Fprintf(os.Stderr, "::warning::assert-image-fresh: template-ref %q is not a SHA and could not be resolved to one — cannot compare against baked dev build %q; skipping.\n", templateRef, baked)
				return nil
			}
			compareTo = sha
		}
		if !shaPrefixMatch(bakedSHA, compareTo) {
			return imageSkewError(baked, templateRef, compareTo)
		}
	} else { // release image — version is a tag
		if refIsSHA {
			fmt.Fprintf(os.Stderr, "::warning::assert-image-fresh: baked llz is release %q but template-ref is a SHA %q — cannot compare; skipping.\n", baked, templateRef)
			return nil
		}
		if baked != templateRef {
			return imageSkewError(baked, templateRef, templateRef)
		}
	}
	fmt.Printf("assert-image-fresh: OK — baked llz %q matches template-ref %q.\n", baked, templateRef)
	return nil
}

// shaPrefixMatch reports whether two git object names refer to the same commit,
// tolerating a short SHA on either side (one must be a prefix of the other).
func shaPrefixMatch(a, b string) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}

// imageSkewError reports the skew. pinCommit is what templateRef RESOLVES to — the
// same string when the pin is already a sha, the tag's commit when it is a tag —
// and is what the remediation pins TF_IMAGE to.
//
// It names BOTH directions because both are fatal and they fail differently. An
// image that LAGS the pin lacks commands/flags the checked-out workflow calls
// ('unknown flag', or a gate that silently no-ops). An image that LEADS it renders
// manifests the pinned llz did not — which surfaces one step later as `llz render
// --check` drift whose own advice ("run `llz render`") is a dead end, since the
// operator's local llz IS the pinned release and re-rendering changes nothing.
// Whoever reads this must be able to tell which one they have.
func imageSkewError(baked, templateRef, pinCommit string) error {
	pinned := templateRef
	if pinCommit != templateRef {
		pinned = fmt.Sprintf("%s (= %s)", templateRef, pinCommit)
	}
	//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline and further remediation lines
	return fmt.Errorf("image/template skew: the ci-tofu image's baked llz is %q but this instance pins template ref %s.\n"+
		"  These must name the same commit. If the image LAGS the pin it lacks any llz command/flag added since it was\n"+
		"  built, so this run fails later with a cryptic 'unknown flag'/'unknown command' or a silently no-op'd gate. If it\n"+
		"  LEADS the pin it renders manifests the pinned llz did not, so the next step fails on `llz render --check` drift\n"+
		"  that re-rendering locally cannot fix.\n"+
		"  Fix — pin the image to the commit the template pin names:\n"+
		"      gh variable set TF_IMAGE     --repo <owner>/<instance> --body ghcr.io/<org>/ci-tofu:sha-%s\n"+
		"      gh variable set KUBE_IMAGE   --repo <owner>/<instance> --body ghcr.io/<org>/ci-kubernetes:sha-%s\n"+
		"  (`llz tokens` computes exactly these for an unset variable.) The alternative is to move the PIN to the image's\n"+
		"  commit instead — `llz upgrade` — but that is a template upgrade, not a pin fix; do it deliberately.",
		baked, pinned, pinCommit, pinCommit)
}
