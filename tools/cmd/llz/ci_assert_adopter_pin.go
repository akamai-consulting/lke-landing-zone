package main

// ci_assert_adopter_pin.go implements `llz ci assert-adopter-pin` — the release
// gate that stands up the shape a REAL ADOPTER is in, which nothing else does.
//
// THE GAP THIS CLOSES. Every e2e run pins the throwaway instance to
// `${GITHUB_SHA}` (e2e-instantiate.yml renders `<@ llz_version @>` to it) and pins
// TF_IMAGE to `ci-tofu:sha-<that same sha>` (`llz ci pin-instance-images`). Three
// legs, one commit, by construction. An adopter has none of that: copier pins them
// at a release TAG, and their TF_IMAGE comes from whatever `llz tokens` computed.
//
// So the e2e harness exercised exactly the one configuration in which the image
// pin cannot be wrong, and the configuration every real instance runs was tested
// by nobody. It broke in the field: `llz tokens` computed `ci-tofu:<versionpins.CITofuTag>`, a
// tag build-images.yml republishes on every push to main, so a release-pinned tree
// ran main's llz and the adopter's first pipeline died on `llz render --check`
// drift they could not resolve. Green e2e the whole time.
//
// WHAT IT ASSERTS, at the moment a release candidate is cut:
//
//  1. the pin resolves — tag → commit
//  2. `llz tokens` computes an IMMUTABLE image pin naming that same commit, for
//     both images. This is the check that fails on the pre-fix code: at v0.0.39 it
//     computed `ci-tofu:1.12.5`, which is not `sha-b9fe2721…`
//  3. both images are actually PUBLISHED. New exposure, and worth stating plainly:
//     now that adopters pin `sha-<release commit>`, a release whose commit never
//     got a successful build-images run hands every new adopter an unpullable
//     image. Cheap to check, catastrophic to miss
//  4. `assert-image-fresh` accepts a binary stamped at that commit against the tag
//     pin, and REJECTS one stamped elsewhere. The tag-vs-dev-sha comparison is the
//     path an adopter's every CI job takes and e2e's takes never
//
// It is deliberately cloud-free — a handful of HTTP requests, no cluster — so it
// runs in the lane's fast pre-flight job and gates the release before any spend.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/sustain"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
)

func ciAssertAdopterPinCmd() *cobra.Command {
	var ref, repo string
	c := &cobra.Command{
		Use:   "assert-adopter-pin",
		Short: "fail if an instance scaffolded at a release tag would not run the matching ci images (adopter-shape gate)",
		Long: "Stands up the pin an ADOPTER gets — a release tag, not the commit sha every\n" +
			"e2e run uses — and asserts the whole chain holds: the tag resolves to a commit,\n" +
			"`llz tokens` computes an immutable TF_IMAGE/KUBE_IMAGE naming that commit, both\n" +
			"images are published, and `assert-image-fresh` accepts a binary built at that\n" +
			"commit while rejecting one built elsewhere.\n\n" +
			"Defaults to the template repo's latest release. Cloud-free; intended for the\n" +
			"release-e2e lane's pre-flight job, ahead of any cluster spend.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runAssertAdopterPin(firstNonEmpty(repo, templatecommit.InstanceTemplateRepo()), ref)
		},
	}
	c.Flags().StringVar(&ref, "ref", "", "release tag to check (default: the template repo's latest release)")
	c.Flags().StringVar(&repo, "template-repo", "", "template repo <owner>/<name> (default: this instance's, else "+sustain.DefaultTemplateRepo+")")
	return c
}

func runAssertAdopterPin(templateRepo, ref string) error {
	if ref == "" {
		latest, ok := latestReleaseTag(templateRepo)
		if !ok {
			return fmt.Errorf("cannot resolve %s's latest release to check — pass --ref <tag> "+
				"(this gate needs a release tag; it is asserting the shape an adopter scaffolds into)", templateRepo)
		}
		ref = latest
	}
	fmt.Printf("assert-adopter-pin: %s @ %s\n", templateRepo, ref)

	// 1. The pin must resolve. Unlike assert-image-fresh — which degrades to a
	//    warning because it cannot tell "skewed" from "unreachable" — an
	//    unresolvable ref IS a failure here: this gate's entire job is to answer
	//    this question, so being unable to ask it is a failed gate, not a pass.
	commit, ok := templatecommit.Resolve(templateRepo, ref)
	if !ok {
		return fmt.Errorf("could not resolve %s@%s to a commit — the gate cannot verify what an adopter "+
			"scaffolding at this tag would run", templateRepo, ref)
	}
	fmt.Printf("  ✓ %s resolves to %s\n", ref, commit)

	// 2. What `llz tokens` would write into the adopter's repo variables.
	//
	// WAIT FIRST, on the release path. This gate fires on `release: prereleased`,
	// and a maintainer publishing the candidate minutes after the merge can easily
	// beat build-images.yml to the finish — the ci images for the release commit are
	// then genuinely absent, and leg 2 would fail a perfectly good release with
	// "would not pin", pointing at the pin computation rather than at a build that
	// is still running. The instantiate job already treats this as a wait rather than
	// an error (`pin-instance-images --build-if-missing`); so does this. Costs
	// nothing when the images are already there — the first check is immediate.
	waitForCIImages(commit)
	// ForCommit: leg 1 already resolved this tag. Re-resolving here would make the
	// verdict depend on a second round-trip, and a blip on it reported "could not
	// resolve" as a pin-computation failure — blaming the code for the network.
	tfImage, kubeImage, pinned, why := templatecommit.ComputeImageVarsForCommit(commit, ref)
	if !pinned {
		//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline and further remediation lines
		return fmt.Errorf("`llz tokens` would not pin an instance scaffolded at %s to an immutable image: %s.\n"+
			"  It would compute TF_IMAGE=%s, KUBE_IMAGE=%s — version tags that build-images.yml republishes on\n"+
			"  every push to main. An instance pinned at %s would then run main's llz against %s's rendered\n"+
			"  manifests and fail its FIRST pipeline run on `llz render --check` drift. That fallback is correct\n"+
			"  behaviour for an OLD release (see computeCIImageVars), but it is not acceptable for one being cut:\n"+
			"  publish the ci images for %s, then re-run this gate.",
			ref, why, tfImage, kubeImage, ref, ref, commit)
	}
	for _, im := range []struct{ name, ref string }{{"TF_IMAGE", tfImage}, {"KUBE_IMAGE", kubeImage}} {
		if want := "sha-" + commit; !strings.HasSuffix(im.ref, ":"+want) {
			return fmt.Errorf("%s would be %s, which does not name the commit %s points at (%s) — "+
				"the baked llz would not be the one that rendered the adopter's tree", im.name, im.ref, ref, want)
		}
		fmt.Printf("  ✓ %s = %s\n", im.name, im.ref)
	}

	// 3. Report the publication evidence. An image that is definitively ABSENT has
	//    already failed above — computeCIImageVars refuses to pin one, so `pinned`
	//    would be false and `why` would name it. What is left to distinguish here is
	//    "confirmed published" from "the registry never answered", and those must not
	//    read the same in a release log: the second is a check that did not happen.
	//
	//    Not a failure, though. A GHCR blip must not block a release the other three
	//    legs vouched for, and the pull itself is the backstop — an absent image
	//    fails the first job with `manifest unknown`, which is unambiguous.
	for _, im := range []struct{ name, ref string }{{"TF_IMAGE", tfImage}, {"KUBE_IMAGE", kubeImage}} {
		if published, asked := templatecommit.ImagePublished(im.ref); published && asked {
			fmt.Printf("  ✓ %s is published\n", im.ref)
			continue
		}
		fmt.Fprintf(os.Stderr, "::warning::assert-adopter-pin: could not confirm %s is published (registry unreachable) — that leg is UNVERIFIED, not passed.\n", im.ref)
	}

	// 4. The comparison an adopter's every CI job makes. Asserting the NEGATIVE
	//    matters as much as the positive: a guard that accepts everything would
	//    satisfy the positive case and is exactly what shipped before.
	// assertImageFreshResolved, not runAssertImageFresh: leg 1 already resolved this
	// tag, so re-resolving would spend two more round-trips AND make the verdict
	// network-dependent. A blip on the NEGATIVE call degrades to warn-and-pass,
	// which reads here as "the guard accepted an unrelated commit" — a hard, false
	// failure manufactured by a transient error.
	if err := assertImageFreshResolved("dev-"+commit, ref, commit); err != nil {
		return fmt.Errorf("assert-image-fresh rejects the image an adopter at %s would correctly be running: %w", ref, err)
	}
	if err := assertImageFreshResolved("dev-"+foreignCommit(commit), ref, commit); err == nil {
		//lint:ignore ST1005 multi-line operator diagnostic: the period precedes an embedded newline explaining the consequence
		return fmt.Errorf("assert-image-fresh ACCEPTED a binary built at an unrelated commit against the %s pin.\n"+
			"  The skew guard is not guarding: an adopter whose TF_IMAGE drifts off their pin would get no warning,\n"+
			"  which is how this class reached a live instance the first time.", ref)
	}
	fmt.Printf("  ✓ assert-image-fresh accepts dev-%.12s and rejects a foreign build\n", commit)

	fmt.Printf("assert-adopter-pin: OK — an instance scaffolded at %s runs the llz that rendered it.\n", ref)
	return nil
}

// foreignCommit returns a well-formed sha that is definitively NOT commit, for the
// negative half of the guard check. Derived from commit (not a constant) so it
// cannot accidentally BE the commit under test on some future release.
func foreignCommit(commit string) string {
	flipped := []byte(commit)
	for i, b := range flipped {
		switch {
		case b >= '0' && b <= '8', b >= 'a' && b <= 'e':
			flipped[i] = b + 1
		default:
			flipped[i] = '0'
		}
	}
	return string(flipped)
}

// latestReleaseTag is the template repo's most recent published release — the tag
// an adopter running `llz new` today would be scaffolded at.
//
// Package var so tests substitute the round-trip.
// `releases/latest` EXCLUDES prereleases and drafts, which is exactly right here
// and must not be "fixed": a prerelease is not what an adopter scaffolds into.
// `llz new` / `llz self-update` skip prereleases (selfupdate.go) — that is the
// whole reason a release candidate can be published without being consumable —
// so the tag this returns is the one an adopter running `llz new` today gets.
//
// Bounded HTTP rather than `gh api`, for the same reason templatecommit.Resolve
// dropped it: execOutput has no timeout, and this runs in a gate.
var latestReleaseTag = func(repo string) (string, bool) {
	if repo == "" {
		return "", false
	}
	req, err := http.NewRequest(http.MethodGet, templatecommit.GithubAPIBase+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if t := templatecommit.GithubToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := (&http.Client{Timeout: templatecommit.HTTPAskTimeout}).Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return "", false
	}
	tag := strings.TrimSpace(r.TagName)
	return tag, tag != ""
}

// The publish-wait budget: 20 minutes, deliberately COPIED from
// `pin-instance-images --timeout` (1200s) rather than guessed. That flag exists to
// wait for exactly these images from exactly this build, so any shorter budget
// here is an assertion that build-images.yml is faster than the repo's own
// measurement of it — and being wrong means failing a good release, which costs a
// re-cut and a re-run of the whole lane.
//
// It is also free in wall-clock terms: this job runs in PARALLEL with instantiate,
// which is already waiting on the same images with the same budget. The lane's
// long pole does not move.
//
// Package vars so tests do not sleep. Every test in the gate's file installs a
// no-op sleep — without one, any case that stubs an image as unpublished burns the
// full budget and the unit test looks hung. Found the hard way.
var (
	adopterPinPublishRetries = 40
	adopterPinPublishDelay   = 30 * time.Second
	adopterPinSleep          = func(d time.Duration) { time.Sleep(d) }
)

// waitForCIImages polls until both ci images for commit are published, the budget
// is spent, or the registry stops answering.
//
// Returns nothing: it is a WAIT, not a check. Whether the images are there is
// still decided downstream by templatecommit.ComputeImageVarsForCommit, which owns that verdict
// and its message — duplicating the decision here would mean two places that can
// disagree about the same fact.
func waitForCIImages(commit string) {
	images := []string{
		templatecommit.CIImageRef(templateid.DefaultOrg, "ci-tofu", "sha-"+commit),
		templatecommit.CIImageRef(templateid.DefaultOrg, "ci-kubernetes", "sha-"+commit),
	}
	for attempt := 0; ; attempt++ {
		missing := ""
		for _, im := range images {
			published, asked := templatecommit.ImagePublished(im)
			// !asked means the registry never answered. Waiting on that is pointless —
			// polling an unreachable endpoint ten more times tells us nothing new, and
			// the downstream check treats "could not ask" as "do not downgrade" anyway.
			if !asked {
				return
			}
			if !published {
				missing = im
				break
			}
		}
		if missing == "" || attempt >= adopterPinPublishRetries {
			return
		}
		if attempt == 0 {
			fmt.Printf("  … %s is not published yet — waiting up to %s for build-images.yml\n",
				missing, time.Duration(adopterPinPublishRetries)*adopterPinPublishDelay)
		}
		adopterPinSleep(adopterPinPublishDelay)
	}
}
