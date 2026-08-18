package templatecommit

// cobra_assert_image_fresh.go implements `llz ci assert-image-fresh` — a fast
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
// IT NEEDED NO NEW DECLARATION. This extension already reads
// "resolve the pinned template commit and report CI image refs that have drifted
// from it" and already binds assertion:configured[read-repo, cloud-read] — the
// command was simply still in package main. hexSHARe came out on arrival: this
// package had the original.
//
// The ref is read from the instance's own .copier-answers.yml rather than passed
// in as a workflow input: a hand-maintained input is a third pin that can skew
// from the other two, which is the very failure this guard exists to catch.

import (
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/pincoherence"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/spf13/cobra"
)

func AssertImageFreshCmd() *cobra.Command {
	var templateRef string
	c := &cobra.Command{
		Use:   "assert-image-fresh",
		Short: "fail if the baked llz is older than the instance's pinned template ref (image/source skew guard)",
		Long: "Compares the ci-tofu image's baked llz build (internal/cli.Version, stamped\n" +
			"`dev-<github.sha>` for dev images or a release tag) against the ref this\n" +
			"instance pins (.copier-answers.yml). A dev image's SHA must match the commit\n" +
			"the pin names — a TAG pin is resolved to its commit first, so tag-vs-SHA is\n" +
			"compared rather than skipped. A release image's tag must equal the pinned tag.\n" +
			"On mismatch it FAILS with an actionable message (the exact TF_IMAGE/KUBE_IMAGE\n" +
			"re-pin). When it cannot compare at all (an unstamped local build, an\n" +
			"unresolvable ref, a release image against a SHA pin) it prints a SKIPPED verdict\n" +
			"and passes — it never blocks on evidence it does not have, and never reports OK\n" +
			"on evidence it does not have either. In GitHub Actions an unstamped build is a\n" +
			"FAILURE, not a skip: there llz comes from the pinned ci image, and every\n" +
			"published image stamps its version.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Check the pin against ITSELF before comparing it to the image. This
			// guard is the instance's only CI-run reader of .copier-answers.yml, and
			// `llz lint` does NOT run in an instance's CI (pre-commit + template CI
			// only) — so without this call the skew reaches a cluster whenever the
			// operator commits the upgrade with --no-verify. An explicit
			// --template-ref overrides the pin, so there is nothing to hold to it.
			if templateRef == "" {
				if err := pincoherence.Assert("."); err != nil {
					return err
				}
			}
			return runAssertImageFresh(Version, firstNonEmpty(templateRef, answers.PinnedTemplateRef()), InstanceTemplateRepo())
		},
	}
	c.Flags().StringVar(&templateRef, "template-ref", "", "override the ref compared against the baked llz build (default: the instance's pin)")
	return c
}

// runAssertImageFresh resolves the pin if it needs resolving, then compares.
//
// templateRepo is passed in rather than read from the instance inside the
// comparison, because callers do not all live in one: `llz ci assert-adopter-pin`
// runs from the TEMPLATE repo and must resolve against the repo it was given.
// Reading `.copier-answers.yml` there returns the first-party default, so on a
// FORK the release gate would have resolved its own release tag against upstream —
// and a tag that upstream does not have resolves to nothing, which the gate then
// reports as "the skew guard is not guarding".
func runAssertImageFresh(bakedVersion, templateRef, templateRepo string) error {
	templateRef = strings.TrimSpace(templateRef)
	if templateRef == "" {
		return fmt.Errorf("cannot resolve the template ref to compare against: no .copier-answers.yml in the working directory (run from an instance checkout, or pass --template-ref)")
	}
	// A TAG pin against a dev-SHA image used to warn and skip. That skip covered the
	// DEFAULT new-adopter shape — copier pins a release tag while TF_IMAGE floats on
	// a tag main republishes — so the guard passed on precisely the instances it
	// exists to protect. Resolve the tag to the commit it names and compare the two
	// as commits. See template_commit.go.
	pinCommit := templateRef
	if isDevBuild(bakedVersion) && !hexSHARe.MatchString(templateRef) {
		sha, ok := Resolve(templateRepo, templateRef)
		if !ok {
			return skipImageFresh(fmt.Sprintf("template-ref %q is not a SHA and could not be resolved to one, so it cannot be compared against baked dev build %q", templateRef, strings.TrimSpace(bakedVersion)))
		}
		pinCommit = sha
	}
	skip, err := assertImageFreshResolved(bakedVersion, templateRef, pinCommit)
	if err != nil {
		return err
	}
	if skip != "" {
		return skipImageFresh(skip)
	}
	fmt.Printf("assert-image-fresh: OK — baked llz %q matches template-ref %q.\n", strings.TrimSpace(bakedVersion), templateRef)
	return nil
}

// skipImageFresh emits the verdict of a run that could not compare anything, and
// is the ONLY thing such a run prints.
//
// It exists because the skip used to be a warning on stderr and the pass a bare
// `return nil` — so the caller ran on and printed OK, on the very value the
// warning had just called unusable. A live instance logged both, one after the
// other (#428), and the OK came second, so that is what the run read as: an
// unstamped `dev` build reported as MATCHING the commit 2de83ec2… — a match
// asserted between a build marker and a commit, which is not a question with a
// true answer. A gate reporting freshness it has just said it cannot establish is
// worse than no gate.
//
// Every skip path therefore RETURNS this call, and the comparison itself returns
// its reason rather than printing one, so no path can reach the OK print without
// having actually compared something.
func skipImageFresh(reason string) error {
	fmt.Fprintf(os.Stderr, "::warning::assert-image-fresh: %s — image/template freshness is NOT verified by this run.\n", reason)
	fmt.Printf("assert-image-fresh: SKIPPED — %s. Nothing was proven about image/template freshness.\n", reason)
	return nil
}

// isDevBuild reports whether a baked version is a `dev-<sha>` stamp.
func isDevBuild(baked string) bool {
	return strings.HasPrefix(strings.TrimSpace(baked), "dev-")
}

// assertImageFreshResolved is the comparison itself, with the pin ALREADY resolved
// to a commit (pinCommit == templateRef when the ref is a sha, or when the baked
// build is a release tag and there is nothing to resolve).
//
// It returns THREE outcomes, not two: skip == "" && err == nil means the versions
// were compared and agree; err != nil means they were compared and disagree; a
// non-empty skip means they could not be compared, and names why. The skip is
// RETURNED rather than printed so that one caller decides what the run's single
// verdict line says — printing it here is what let a skip and an OK be emitted for
// the same value (#428). No caller may treat a skip as a pass.
//
// Pure — no network, no filesystem, aside from reading GITHUB_ACTIONS. That is what
// lets `llz ci assert-adopter-pin` exercise this logic with the commit it resolved
// in its own first step instead of paying for two more round-trips. Those
// round-trips were not just waste: a blip on the NEGATIVE one degraded to
// warn-and-pass, which the gate reads as "the guard accepted an unrelated commit"
// and reports as a hard failure. A transient network error must not be able to
// manufacture that verdict.
func assertImageFreshResolved(bakedVersion, templateRef, pinCommit string) (skip string, err error) {
	baked := strings.TrimSpace(bakedVersion)
	if baked == "" || baked == "dev" {
		// UNSTAMPED IN CI IS A STAMPING BUG, NOT A LOCAL DEV BUILD. Every invocation
		// of this verb in a delivered workflow runs inside `container: ${{
		// vars.TF_IMAGE }}`, and every image build-images.yml publishes stamps
		// internal/cli.Version from --build-arg LLZ_VERSION. So a binary that both
		// carries THIS code and reports no stamp did not come from a published ci
		// image — the one way that can happen is the stamp regressing, which it has:
		// the linker silently ignores `-X <path>.Var=` for a symbol it cannot
		// resolve, so dockerfiles/Dockerfile kept stamping `main.version` for months
		// after the command tree left package main, every ci image baked "dev", and
		// this guard took its warn-and-pass arm on every adopter it exists to protect
		// (PR #433). Skipping there means the skew guard is off and nothing says so
		// louder than an annotation nobody reads.
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			return "", unstampedInCIError(baked)
		}
		return fmt.Sprintf("the baked llz version is unstamped (%q), which is what a local `go build` without the release ldflags produces", bakedVersion), nil
	}
	if bakedSHA := strings.TrimPrefix(baked, "dev-"); bakedSHA != baked { // dev image — compare SHAs
		if !shaPrefixMatch(bakedSHA, pinCommit) {
			return "", imageSkewError(baked, templateRef, pinCommit)
		}
		return "", nil
	}
	// Release image — the baked version is a tag, so the pin must be one too.
	if hexSHARe.MatchString(templateRef) {
		return fmt.Sprintf("the baked llz is release %q but template-ref is a SHA %q, and a tag cannot be compared to a commit without resolving one of them", baked, templateRef), nil
	}
	if baked != templateRef {
		return "", imageSkewError(baked, templateRef, templateRef)
	}
	return "", nil
}

// unstampedInCIError is the failure an unstamped build takes in CI. It has to name
// BOTH causes, because the remediation differs: an image that is simply old wants a
// re-pin, while a current image that reports "dev" means the stamping itself broke
// and no re-pin will help.
func unstampedInCIError(baked string) error {
	//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline and further remediation lines
	return fmt.Errorf("the baked llz carries no version stamp (%q), so image/template freshness cannot be established at all.\n"+
		"  This is a CI run, where llz comes from the pinned ci image — and every image build-images.yml publishes stamps\n"+
		"  its version (`--build-arg LLZ_VERSION=dev-<sha>`). An unstamped one is therefore either not a published image, or\n"+
		"  a build whose stamp landed nowhere: the Go linker SILENTLY IGNORES `-X <path>.Var=` for a symbol it cannot\n"+
		"  resolve, which is how every ci image baked \"dev\" for months while this guard warned and passed.\n"+
		"  Fix — if TF_IMAGE/KUBE_IMAGE are old, re-pin them to a published build:\n"+
		"      llz tokens --env <deployment> --yes\n"+
		"  If they already name a `sha-<commit>` image that build-images.yml published, the stamping is broken rather than\n"+
		"  the pin: check the `-X` path in dockerfiles/Dockerfile against internal/cli.Version — version_stamp_test.go pins\n"+
		"  every stamping site, and a new one added outside its list is exactly this failure.", baked)
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
