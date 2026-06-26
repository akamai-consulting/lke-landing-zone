package main

// ci_pin_images.go implements `llz ci pin-instance-images` — the release-e2e
// instantiate job's image-pin logic, moved out of inline workflow bash into
// unit-tested Go (the repo's untestable-loc design principle).
//
// It points the throwaway instance repo's TF_IMAGE / KUBE_IMAGE variables at the
// CI images built from THIS commit, so the baked `llz` binary can never drift
// from the workflow YAML the instantiate job renders at the same commit (the
// recurring "llz: unknown flag" / stale-binary e2e failures). build-images only
// runs when tools/dockerfiles change, so a per-commit `sha-<sha>` image exists
// ONLY for binary-changing commits: pin the exact sha when one built (waiting for
// a build cut just before release to finish publishing), else pin `:latest` (the
// most recent build = the unchanged binary's image).
//
// I/O is behind package-var seams (pinGH / pinManifestExists / pinDockerLogin /
// pinSleep) so the decision logic + wait loop are tested without a registry.

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// pinImage maps an instance repo variable to the ci image whose tag it pins.
type pinImage struct {
	Var  string // instance repo variable name (TF_IMAGE / KUBE_IMAGE)
	Name string // ci image name under the GHCR owner (ci-terraform / ci-kubernetes)
}

var pinImages = []pinImage{
	{Var: "TF_IMAGE", Name: "ci-terraform"},
	{Var: "KUBE_IMAGE", Name: "ci-kubernetes"},
}

// Seams (package vars) so tests drive the flow without gh/docker/a registry.
var (
	// pinGH runs `gh <args>` with GH_TOKEN set to token (template vs instance
	// calls need different credentials, so each names its own).
	pinGH = func(token string, args ...string) ([]byte, error) {
		cmd := exec.Command("gh", args...)
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
		return cmd.Output()
	}
	// pinManifestExists reports whether an image tag is published+pullable.
	pinManifestExists = func(image string) bool {
		return exec.Command("docker", "manifest", "inspect", image).Run() == nil
	}
	// pinDockerLogin authenticates to ghcr.io so manifest inspect can read this
	// org's (private) images.
	pinDockerLogin = func(token, user string) error {
		cmd := exec.Command("docker", "login", "ghcr.io", "-u", user, "--password-stdin")
		cmd.Stdin = strings.NewReader(token)
		return cmd.Run()
	}
	pinSleep = func(d time.Duration) { time.Sleep(d) }

	// pinBuildInProgress reports whether a "Build Container Images" run for sha is
	// currently queued/running — so we wait for it instead of starting a duplicate.
	pinBuildInProgress = func(token, templateRepo, sha string) bool {
		out, err := pinGH(token, "api",
			fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100", templateRepo, sha),
			"--jq", `[.workflow_runs[] | select(.name=="Build Container Images") | select(.status=="queued" or .status=="in_progress")] | length`)
		return err == nil && parseBuildCount(out) > 0
	}
	// pinTriggerBuild kicks off the Build Container Images workflow on ref (its
	// HEAD = the commit under test in the e2e flow). Needs an actions:write token.
	pinTriggerBuild = func(token, templateRepo, ref string) error {
		cmd := exec.Command("gh", "workflow", "run", "build-images.yml",
			"--repo", templateRepo, "--ref", ref, "-f", "image=all")
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
		return cmd.Run()
	}
)

func ciPinInstanceImagesCmd() *cobra.Command {
	var instance, owner, templateRepo, sha, ref string
	var interval, timeout int
	var buildIfMissing bool
	c := &cobra.Command{
		Use:   "pin-instance-images",
		Short: "pin the e2e instance's TF_IMAGE/KUBE_IMAGE to this commit's ci images",
		Long: "Points the instance repo's TF_IMAGE / KUBE_IMAGE variables at the ci-terraform\n" +
			"/ ci-kubernetes images for --sha, so the baked llz binary can't drift from the\n" +
			"rendered workflow. If this commit triggered a Build Container Images run, waits\n" +
			"for its sha- image to publish and pins the exact sha; otherwise pins :latest\n" +
			"(the binary is unchanged). Reads GH_TOKEN_TEMPLATE (this repo's runs + GHCR\n" +
			"reads) and GH_TOKEN_INSTANCE (instance variable writes) from the environment.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runPinInstanceImages(pinOpts{
				instance: instance, owner: strings.ToLower(owner), templateRepo: templateRepo,
				sha: sha, ref: ref, actor: os.Getenv("GITHUB_ACTOR"),
				templateToken: os.Getenv("GH_TOKEN_TEMPLATE"), instanceToken: os.Getenv("GH_TOKEN_INSTANCE"),
				interval:       time.Duration(interval) * time.Second,
				retries:        timeout / max1(interval),
				buildIfMissing: buildIfMissing,
			})
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "instance repo owner/name (TF_IMAGE/KUBE_IMAGE are set here)")
	c.Flags().StringVar(&owner, "owner", "", "GHCR namespace owner (this repo's org)")
	c.Flags().StringVar(&templateRepo, "template-repo", "", "this (template) repo owner/name — queried for the build run")
	c.Flags().StringVar(&sha, "sha", "", "the commit whose images to pin")
	c.Flags().StringVar(&ref, "ref", "", "branch/tag to (re)trigger Build Container Images on with --build-if-missing (its HEAD must be --sha)")
	c.Flags().BoolVar(&buildIfMissing, "build-if-missing", false, "if this commit's images should exist but are missing (build failed/incomplete), trigger Build Container Images and wait — instead of pinning a stale image or failing")
	c.Flags().IntVar(&interval, "interval", 20, "seconds between manifest polls while waiting for a sha image")
	c.Flags().IntVar(&timeout, "timeout", 1200, "max seconds to wait for a just-built sha image to publish")
	return c
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

type pinOpts struct {
	instance, owner, templateRepo, sha, ref, actor string
	templateToken, instanceToken                   string
	interval                                       time.Duration
	retries                                        int
	buildIfMissing                                 bool
}

func runPinInstanceImages(o pinOpts) error {
	for _, v := range []struct{ name, val string }{
		{"--instance", o.instance}, {"--owner", o.owner}, {"--template-repo", o.templateRepo},
		{"--sha", o.sha}, {"GH_TOKEN_TEMPLATE", o.templateToken}, {"GH_TOKEN_INSTANCE", o.instanceToken},
	} {
		if v.val == "" {
			return fmt.Errorf("pin-instance-images: %s is required", v.name)
		}
	}
	if err := pinDockerLogin(o.templateToken, o.actor); err != nil {
		return fmt.Errorf("docker login ghcr.io failed: %w", err)
	}

	built, err := commitBuiltImages(o.templateToken, o.templateRepo, o.sha)
	if err != nil {
		return err
	}

	// This commit changed the image sources (a build ran) but its images aren't
	// all published — the build failed or is still running. With --build-if-missing
	// (the workflow_dispatch e2e path), kick a fresh build unless one is already
	// running, then fall through to the per-image wait below. Without it, the wait
	// loop alone fails loud if the image never publishes. Either way we never pin a
	// stale image. One build covers every pinImages entry.
	if built && o.buildIfMissing && anyShaImageMissing(o.owner, o.sha) {
		if o.ref == "" {
			return fmt.Errorf("pin-instance-images: --ref is required with --build-if-missing (the branch/tag to build)")
		}
		if pinBuildInProgress(o.templateToken, o.templateRepo, o.sha) {
			fmt.Printf("Build Container Images already running for %.8s — waiting for it to publish.\n", o.sha)
		} else {
			fmt.Printf("Images for %.8s are missing and no build is in progress — triggering Build Container Images on %s.\n", o.sha, o.ref)
			if err := pinTriggerBuild(o.templateToken, o.templateRepo, o.ref); err != nil {
				return fmt.Errorf("could not trigger Build Container Images on %s — GH_TOKEN_TEMPLATE needs actions:write: %w", o.ref, err)
			}
		}
	}

	for _, im := range pinImages {
		base := fmt.Sprintf("ghcr.io/%s/%s", o.owner, im.Name)
		ref := imageRef(base, o.sha, built)
		if built {
			fmt.Printf("Build Container Images ran for %.8s — waiting for %s to publish…\n", o.sha, ref)
			if !waitForManifest(ref, o.retries, o.interval) {
				return fmt.Errorf("%s not published in time — did Build Container Images succeed for %.8s?", ref, o.sha)
			}
		} else if !pinManifestExists(ref) {
			return fmt.Errorf("%s not found in GHCR", ref)
		}
		if _, err := pinGH(o.instanceToken, "variable", "set", im.Var, "--repo", o.instance, "--body", ref); err != nil {
			return fmt.Errorf("could not set %s on %s — GH_TOKEN_INSTANCE needs 'Variables: read and write': %w", im.Var, o.instance, err)
		}
		fmt.Printf("Pinned %s %s=%s\n", o.instance, im.Var, ref)
	}
	return nil
}

// anyShaImageMissing reports whether any pinned image's sha-<sha> tag is not yet
// published — the signal that a build is incomplete or failed.
func anyShaImageMissing(owner, sha string) bool {
	for _, im := range pinImages {
		if !pinManifestExists(fmt.Sprintf("ghcr.io/%s/%s:sha-%s", owner, im.Name, sha)) {
			return true
		}
	}
	return false
}

// imageRef is the tag to pin: the exact sha when this commit built an image,
// else the moving :latest (the unchanged binary's most-recent build). Pure.
func imageRef(base, sha string, built bool) string {
	if built {
		return base + ":sha-" + sha
	}
	return base + ":latest"
}

// commitBuiltImages reports whether a "Build Container Images" run exists for sha
// (i.e. the commit touched tools/dockerfiles, so a per-commit image is/was built).
func commitBuiltImages(token, templateRepo, sha string) (bool, error) {
	out, err := pinGH(token, "api",
		fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100", templateRepo, sha),
		"--jq", `[.workflow_runs[] | select(.name=="Build Container Images")] | length`)
	if err != nil {
		return false, fmt.Errorf("querying Build Container Images runs for %.8s: %w", sha, err)
	}
	return parseBuildCount(out) > 0, nil
}

// parseBuildCount reads the `gh --jq '… | length'` integer. Non-numeric/empty
// output (no runs) counts as 0. Pure.
func parseBuildCount(out []byte) int {
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return n
}

// waitForManifest polls pinManifestExists until the image is published or the
// retry budget is spent; the first check is immediate. Returns whether it appeared.
func waitForManifest(image string, retries int, delay time.Duration) bool {
	for attempt := 0; ; attempt++ {
		if pinManifestExists(image) {
			return true
		}
		if attempt >= retries {
			return false
		}
		pinSleep(delay)
	}
}
