package templatecommit

// cobra_assert_release_image.go implements `llz ci assert-release-image` — the
// preflight that refuses to publish a release whose image was never built.
//
// THE INCIDENT (v0.0.44). The release workflow retags the
// commit's `sha-<sha>` image as `vX.Y.Z`. For v0.0.44 that image did not exist, so
// `imagetools create` failed:
//
//	ERROR: ghcr.io/akamai-consulting/llz:sha-1276c08f… : not found
//
// The retag step was written to fail loudly for exactly this reason, and it did.
// The problem is WHEN. `image-tag` declares no `needs:`, so it runs beside the
// build — and by the time it failed, "Attach binaries to the release" had already
// succeeded and "Documented install flow" had passed against them. The release was
// published, installable, and advertised a `:vX.Y.Z` image that does not exist.
// Every instance pinning it renders that tag into its carved apps and gets an
// ImagePullBackOff, which is the failure this whole job exists to prevent —
// reintroduced by a job ordering rather than by the check being absent.
//
// A GATE THAT RUNS AFTER THE IRREVERSIBLE STEP IS A REPORT, NOT A GATE. This verb
// is the same question asked BEFORE anything is attached, so the release ends up
// with no assets — visibly incomplete — rather than complete and subtly wrong. A
// release with no binaries is re-cut in a minute; a release with a missing image
// tag is found by an adopter.
//
// IT WAITS, BECAUSE THE FAILURE WAS A RACE AND NOT A MISSING BUILD. Measured on
// the incident: build-images for that commit started 00:28:49Z and was still
// in_progress when the release fired at 00:29:55Z — 66 seconds behind it. The
// image was published minutes later and is in the registry now. A check that
// merely failed fast would have been correct and useless: it would turn every
// release cut promptly after a merge into a manual re-run, which trains people to
// re-run it without reading it.
//
// The budget is sized from measurement, not taste: build-images runs completed in
// 6-9 minutes across the last eight, so the default 60 attempts at 15s covers a
// slow run with margin, inside the job's own timeout.
//
// BOUNDED BY ATTEMPTS, NOT BY A DEADLINE. A loop that polls until time.Now passes
// a deadline spins at full speed the moment its clock seam is a no-op, which is
// how a poll loop in this repo once burned its whole budget in milliseconds. The
// count is the budget.
//
// WHY IT STILL FAILS CLOSED at the end. assert-image-fresh draws the line
// differently, and the difference is the SOURCE of the missing evidence, not the
// fact of it: that verb fails on an unusable stamp — a defect in an artifact this
// repo builds — and only skips when a third party will not answer (an unresolvable
// ref), because it runs on every PR and blocking one on a network blip costs more
// than it saves. This one runs once per release, where the cost of a wrong PASS is
// a published artifact an adopter cannot pull, so even the third-party case fails.

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
	"github.com/spf13/cobra"
)

// sleepFor is the seam. Tests substitute it so the poll loop runs at test speed;
// nothing else may read a clock here (see the header).
var sleepFor = time.Sleep

func AssertReleaseImageCmd() *cobra.Command {
	var owner, sha, image string
	var attempts int
	var interval time.Duration
	c := &cobra.Command{
		Use:   "assert-release-image",
		Short: "fail before a release publishes if the commit's image was never pushed",
		Long: "Asserts that ghcr.io/<owner>/<image>:sha-<sha> — the artifact the release\n" +
			"retags as its version tag — actually resolves in the registry.\n\n" +
			"Runs BEFORE the release attaches its binaries, so a commit whose build-images\n" +
			"run never completed produces a visibly empty release rather than a complete one\n" +
			"advertising an image tag that 404s in an adopter's cluster.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return RunAssertReleaseImage(owner, image, sha, attempts, interval, cmd.OutOrStdout())
		},
	}
	f := c.Flags()
	f.StringVar(&owner, "owner", templateid.DefaultOrg, "GHCR owner")
	f.StringVar(&image, "image", "llz", "image name the release retags")
	f.StringVar(&sha, "sha", "", "the release commit (required)")
	f.IntVar(&attempts, "attempts", 60, "how many times to look before giving up (the budget IS the count)")
	f.DurationVar(&interval, "interval", 15*time.Second, "wait between looks")
	return c
}

// RunAssertReleaseImage is the decision, separated from cobra so it is testable
// against a substituted ImagePublished and sleepFor.
func RunAssertReleaseImage(owner, image, sha string, attempts int, interval time.Duration, out io.Writer) error {
	if strings.TrimSpace(sha) == "" {
		return fmt.Errorf("--sha is required: without the release commit this check has nothing to look up, " +
			"and passing it would be a gate that examined nothing")
	}
	if attempts < 1 {
		attempts = 1
	}
	ref := CIImageRef(owner, image, "sha-"+sha)

	var everAsked bool
	for i := 0; i < attempts; i++ {
		published, asked := ImagePublished(ref)
		everAsked = everAsked || asked
		if asked && published {
			if i > 0 {
				fmt.Fprintf(out, "%s appeared after %d look(s).\n", ref, i+1)
			}
			fmt.Fprintf(out, "%s is published — safe to publish the release and retag it.\n", ref)
			return nil
		}
		// No sleep after the last look: it would add a wait to the failure path
		// and delay the report by a whole interval for no extra evidence.
		if i < attempts-1 {
			if i == 0 {
				fmt.Fprintf(out, "%s not in the registry yet — waiting for build-images (up to %d look(s), %s apart).\n",
					ref, attempts, interval)
			}
			sleepFor(interval)
		}
	}

	if !everAsked {
		return fmt.Errorf("could not ask the registry whether %s exists, across %d attempt(s).\n"+
			"  This check FAILS CLOSED rather than assume: a release that publishes an image tag\n"+
			"  nothing can pull is found by an adopter, not by CI. Re-run this job once GHCR is\n"+
			"  reachable — nothing has been published yet", ref, attempts)
	}
	return fmt.Errorf("%s never appeared, after %d attempt(s) over %s.\n"+
		"  The release was cut from a commit whose build-images run has not published it. Nothing\n"+
		"  has been attached to the release yet, which is the point of checking here.\n"+
		"  Fix: check build-images for this commit — a back-to-back run can 403/429 on the GHCR\n"+
		"  push, so confirm it succeeded rather than assuming it never ran. Re-run this release\n"+
		"  workflow once the image is up", ref, attempts, time.Duration(attempts-1)*interval)
}
