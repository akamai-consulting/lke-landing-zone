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
	// THE STAMP IS JUDGED BEFORE THE PIN IS RESOLVED, and the order is load-bearing
	// twice over. A stamp that names no commit cannot be compared to anything, so
	// resolving the tag first would be a network round trip whose answer cannot
	// change the verdict — and, worse, it would reach the comparison with an empty
	// SHA, which prefix-matches every pin (see stampedSHA).
	//
	// This is also the ONE place the CI-vs-local policy is decided. The comparison
	// itself stays a pure function of its arguments, so `llz ci assert-adopter-pin`
	// gets the same answer from it in a GitHub job as on a laptop.
	if why, unusable := unstampedReason(bakedVersion); unusable {
		if inGitHubActions() {
			return unstampedInCIError(bakedVersion)
		}
		return skipImageFresh(why)
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
			// AND THIS ONE STAYS A SKIP IN CI, deliberately, where an unusable stamp
			// does not. The two look alike and are not: an unusable stamp is a defect
			// in an artifact THIS REPO BUILDS, reproducible on every run of that image
			// and fixable by whoever sees it. An unresolvable ref is a third party
			// being unavailable — a GitHub 5xx, a job without GH_TOKEN, or a GHES
			// instance whose anonymous requests hit the 60/hr per-IP limit
			// (template_commit.go). Failing on that hands every adopter a red pipeline
			// whenever api.github.com has a bad minute, which buys no evidence and
			// costs the run. The honest cost is real and worth stating: this arm is
			// the MORE likely of the two to fire, so a persistently red-free instance
			// that never prints OK deserves a look at its GH_TOKEN.
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
	// NAME WHAT WAS ACTUALLY COMPARED, exactly as imageSkewError does. On the default
	// adopter shape the pin is a TAG and the comparison was against the commit it
	// resolves to, so printing the tag alone reports a verdict nobody can audit from
	// the log — which is how `OK — baked llz "dev-" matches template-ref "v0.0.44"`
	// reads plausible while being a match against nothing.
	pinned := templateRef
	if pinCommit != templateRef {
		pinned = fmt.Sprintf("%s (= %s)", templateRef, pinCommit)
	}
	fmt.Printf("assert-image-fresh: OK — baked llz %q matches template-ref %s.\n", strings.TrimSpace(bakedVersion), pinned)
	return nil
}

// inGitHubActions reports whether this process is a CI step.
//
// `!= ""` rather than `== "true"`: five of the six other readers in this binary
// (ghsecret, ghaout, credrotate, pg_probe, sustain/drift) spell it this way, and
// here the spelling has a direction. `== "true"` would send any runner exporting
// `GITHUB_ACTIONS=1` back to the warn-and-pass arm — the guard-off state this file
// exists to eliminate — while ghsecret in the same process masked its secrets as
// though it were CI. The stricter comparison is the fail-open one.
func inGitHubActions() bool { return os.Getenv("GITHUB_ACTIONS") != "" }

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

// stampedSHA returns the commit a `dev-<sha>` stamp names, and whether this is a
// dev stamp at all.
//
// THE SHA IS VALIDATED, and it was not. `strings.TrimPrefix("dev-", "dev-")` is the
// empty string, and shaPrefixMatch treats an empty prefix as a match against every
// pin — so a `dev-` stamp printed `OK — baked llz "dev-" matches template-ref
// "v0.0.44"` against any instance in the world, which is #428's own sentence with a
// different marker in it. `dev-0` matched one commit in sixteen. Nothing produced
// those stamps on purpose; `--build-arg LLZ_VERSION=dev-${SHA}` with an empty SHA
// does, and the surrounding guard is exactly the wrong place to assume a producer
// gets its inputs right.
func stampedSHA(baked string) (string, bool) {
	sha, isDev := strings.CutPrefix(strings.TrimSpace(baked), "dev-")
	return sha, isDev
}

// unstampedReason says why a baked version cannot be compared to anything, and is
// the single definition of "this binary does not know what it was built from".
//
// It is decided from the baked value ALONE — no pin, no network — which is what
// lets runAssertImageFresh ask it before spending a round trip resolving a tag, and
// lets the comparison ask it again for a caller that never goes through there.
//
// The class matters, not just the fact: an unusable stamp is a defect in something
// THIS REPO BUILDS (the image's ldflags), which is why it is fatal in CI, while an
// unresolvable ref is a third party being unavailable, which is not.
func unstampedReason(bakedVersion string) (string, bool) {
	baked := strings.TrimSpace(bakedVersion)
	if baked == "" || baked == "dev" {
		return fmt.Sprintf("the baked llz version is unstamped (%q), which is what a local `go build` without the release ldflags produces", bakedVersion), true
	}
	if sha, isDev := stampedSHA(baked); isDev && !hexSHARe.MatchString(sha) {
		return fmt.Sprintf("the baked llz version %q is malformed — the part after `dev-` is %q, which is not a commit sha, so there is nothing to compare against the pin", bakedVersion, sha), true
	}
	return "", false
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
// Pure — no network, no filesystem, no environment. The CI-vs-local policy lives in
// runAssertImageFresh, one frame up, so this stays a function of its arguments only.
// That is what lets `llz ci assert-adopter-pin` exercise this logic with the commit it resolved
// in its own first step instead of paying for two more round-trips. Those
// round-trips were not just waste: a blip on the NEGATIVE one degraded to
// warn-and-pass, which the gate reads as "the guard accepted an unrelated commit"
// and reports as a hard failure. A transient network error must not be able to
// manufacture that verdict.
func assertImageFreshResolved(bakedVersion, templateRef, pinCommit string) (skip string, err error) {
	baked := strings.TrimSpace(bakedVersion)
	// Checked again here, not only in runAssertImageFresh: assert-adopter-pin calls
	// this function directly, and a comparison that silently accepted an unusable
	// stamp from ONE of its two callers is the whole failure class.
	if why, unusable := unstampedReason(bakedVersion); unusable {
		return why, nil
	}
	if bakedSHA, isDev := stampedSHA(baked); isDev { // dev image — compare SHAs
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

// unstampedInCIError is the failure an unusable stamp takes in CI.
//
// It names all THREE causes because the remediation differs and the wrong one costs
// a day. It also prints the stamp RAW: a padded `"  dev  "` is a quoting bug in the
// build-arg, not a wrong `-X` path, and trimming it away deletes the only evidence
// that distinguishes them.
//
// It does NOT lead with the re-pin. An llz carrying this code came from an image
// built after the stamping fix, so "your image is old" is the one cause that cannot
// produce this message — the re-pin is here for the case where the pin genuinely
// moved, not as the first thing to try.
func unstampedInCIError(baked string) error {
	// No ST1005 waiver here, unlike its two neighbours: this message happens to end
	// on a parenthesis rather than a period, so the check does not fire — and
	// staticcheck rejects a directive that matched nothing, which is how a stale
	// waiver gets noticed rather than accumulating.
	return fmt.Errorf("the baked llz carries no usable version stamp (%q), so image/template freshness cannot be established at all.\n"+
		"  This is a CI run, where a delivered workflow runs llz from the pinned ci image — and every image\n"+
		"  build-images.yml publishes stamps its version (`--build-arg LLZ_VERSION=dev-<sha>`). Three things produce an\n"+
		"  unusable one, in the order worth checking:\n"+
		"    1. THE STAMPING BROKE. The Go linker SILENTLY IGNORES `-X <path>.Var=` for a symbol it cannot resolve, which\n"+
		"       is how every ci image baked \"dev\" for months while this guard warned and passed. Check the `-X` path in\n"+
		"       dockerfiles/Dockerfile against internal/cli.Version; version_stamp_test.go pins every stamping site, and a\n"+
		"       new one added outside its list is exactly this failure. An empty or malformed LLZ_VERSION build-arg lands\n"+
		"       here too — the stamp is then present but names no commit.\n"+
		"    2. THIS llz IS NOT FROM A PUBLISHED IMAGE — a job that builds it from source (`go build ./cmd/llz`) has no\n"+
		"       stamp to read. Such a job has no template pin to check either; it should not be running this verb.\n"+
		"    3. THE PIN MOVED and the images did not. Re-pin them to a published build:\n"+
		"           llz tokens --env <deployment> --yes\n"+
		"       (Least likely of the three here: an image old enough to predate the stamping fix runs an older llz, which\n"+
		"       cannot reach this message at all.)", baked)
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
