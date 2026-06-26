package main

// ci_assert_image_fresh.go implements `llz ci assert-image-fresh` — a fast
// preflight that fails LOUD when the ci-terraform image's baked `llz` is older
// than the workflow's checked-out template-ref.
//
// WHY: the e2e instance pins TF_IMAGE (the container whose baked llz the jobs
// run) and template-ref (the TF roots + workflow source) INDEPENDENTLY, so they
// drift. When the image lags, the checked-out workflow calls llz subcommands/
// flags the baked binary doesn't have — surfacing as a silent no-op readiness
// gate (the AppProject CRD race in PR #86) or a cryptic "unknown flag" ~20 min
// into a run. This guard turns that into a clear failure at the FIRST job.

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
		Use:   "assert-image-fresh --template-ref <ref>",
		Short: "fail if the baked llz is older than the workflow's template-ref (image/source skew guard)",
		Long: "Compares the ci-terraform image's baked llz build (main.version, stamped\n" +
			"`dev-<github.sha>` for dev images or a release tag) against the workflow's\n" +
			"--template-ref. A dev image's SHA must match the template-ref SHA; a release\n" +
			"image's tag must equal the template-ref. On mismatch it FAILS with an\n" +
			"actionable message (republish ci-terraform / re-pin TF_IMAGE). When it cannot\n" +
			"compare (unstamped local build, or a tag-vs-SHA pair) it warns and passes —\n" +
			"this never blocks a legitimately-matched or unverifiable run.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runAssertImageFresh(version, templateRef) },
	}
	c.Flags().StringVar(&templateRef, "template-ref", "", "the ref/SHA the workflow checked the template out at (compared to the baked llz build)")
	_ = c.MarkFlagRequired("template-ref")
	return c
}

func runAssertImageFresh(bakedVersion, templateRef string) error {
	templateRef = strings.TrimSpace(templateRef)
	if templateRef == "" {
		return fmt.Errorf("--template-ref is required")
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
		if !refIsSHA {
			fmt.Fprintf(os.Stderr, "::warning::assert-image-fresh: template-ref %q is not a SHA — cannot compare against baked dev build %q; skipping.\n", templateRef, baked)
			return nil
		}
		if !shaPrefixMatch(bakedSHA, templateRef) {
			return imageSkewError(baked, templateRef)
		}
	} else { // release image — version is a tag
		if refIsSHA {
			fmt.Fprintf(os.Stderr, "::warning::assert-image-fresh: baked llz is release %q but template-ref is a SHA %q — cannot compare; skipping.\n", baked, templateRef)
			return nil
		}
		if baked != templateRef {
			return imageSkewError(baked, templateRef)
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

func imageSkewError(baked, templateRef string) error {
	return fmt.Errorf("image/template skew: the ci-terraform image's baked llz is %q but the workflow checked out template-ref %q.\n"+
		"  The baked binary lacks any llz command/flag added after its build, so this run will fail later with a cryptic\n"+
		"  'unknown flag'/'unknown command' or a silently no-op'd gate. Fix: republish ci-terraform at %s (build-images.yml)\n"+
		"  and re-pin the instance's TF_IMAGE to ghcr.io/<org>/ci-terraform:sha-%s, OR run at a template-ref matching the image.",
		baked, templateRef, templateRef, templateRef)
}
