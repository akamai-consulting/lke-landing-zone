package releasepublish

// ci_pin_images.go implements `llz ci pin-instance-images` — the release-e2e
// instantiate job's image-pin logic, moved out of inline workflow bash into
// unit-tested Go (the repo's untestable-loc design principle).
//
// It points the throwaway instance repo's TF_IMAGE / KUBE_IMAGE variables at the
// CI images built from THIS commit, so the baked `llz` binary can never drift
// from the workflow YAML the instantiate job renders at the same commit (the
// recurring "llz: unknown flag" / stale-binary e2e failures). build-images only
// runs when tools/ or dockerfiles/ change, so a per-commit `sha-<sha>` image exists
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
)

// pinImage maps an instance repo variable to the ci image whose tag it pins.
type pinImage struct {
	Var  string // instance repo variable name (TF_IMAGE / KUBE_IMAGE)
	Name string // ci image name under the GHCR owner (ci-tofu / ci-kubernetes)
}

var pinImages = []pinImage{
	// The VARIABLE keeps its name. TF_IMAGE is set in every adopter repo, so
	// renaming it would break instances that this rename is meant not to break;
	// only the image it points AT changed (ci-terraform -> ci-tofu).
	{Var: "TF_IMAGE", Name: "ci-tofu"},
	{Var: "KUBE_IMAGE", Name: "ci-kubernetes"},
}

// requiredShaImages are images the run CONSUMES for this commit but pins no
// repo VARIABLE at — so they have no entry above, and were therefore neither
// waited for nor counted as "missing" when deciding to trigger a build.
//
// THE ROUND THAT COST. `llz` is the in-cluster image: e2e-instantiate renders
// LLZ_IMAGE_REF to ghcr.io/<owner>/llz:sha-<commit>, every carved Application's
// kustomize images: transformer retags onto it, and Kyverno's
// verify-llz-image-signature policy verifies that exact reference at admission.
// This verb checked ci-tofu and ci-kubernetes, found both published, reported
// success — and the llz image for that commit had never been built. The cluster
// then came up and Kyverno correctly refused FOUR workloads:
//
//	resource Deployment/llz-reconciler/llz-reconciler was blocked …
//	  failed to verify image ghcr.io/akamai-consulting/llz:sha-4481768c…:
//	  MANIFEST_UNKNOWN: manifest unknown
//
// plus obj-proxy's DaemonSet and two CronJobs. No llz-reconciler Deployment
// means no apl-overlay push, so loki-s3-linode-credentials was never built and
// convergence waited out its entire 1200s budget on a downstream symptom, ~40
// minutes and a real cluster after the actual cause.
//
// The verb's own name is why this was invisible: it PINS two variables, so two
// images looked like the whole job. What it really owes the run is "every image
// this commit's cluster will be asked to pull".
var requiredShaImages = []string{"llz"}

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
	// pinBuildFailed reports whether the most recent "Build Container Images" run
	// for sha has already CONCLUDED unsuccessfully.
	//
	// WITHOUT THIS THE WAIT CANNOT TELL A SLOW BUILD FROM A DEAD ONE. waitForManifest
	// polls the registry for an image and nothing else, so a build that failed at
	// its push step looks exactly like one still running: the poll burns its whole
	// budget and then errors "not published in time — did Build Container Images
	// succeed for <sha>?", a question this API call answers in one request.
	//
	// It is not hypothetical. A GHCR secondary rate limit (`You have exceeded a
	// secondary rate limit`) failed the ci-kubernetes push ~4 minutes in, and the
	// e2e lane's instantiate job then sat waiting ~20 more minutes for an image
	// that was never coming — indistinguishable, from the outside, from a hang.
	//
	// Same query shape as pinBuildInProgress above, deliberately: one endpoint, one
	// filter, so the two cannot disagree about which runs count.
	pinBuildFailed = func(token, templateRepo, sha string) (string, bool) {
		out, err := pinGH(token, "api",
			fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100", templateRepo, sha),
			"--jq", `[.workflow_runs[] | select(.name=="Build Container Images") | select(.status=="completed") | select(.conclusion!="success") | .html_url] | first // ""`)
		if err != nil {
			return "", false // could not tell — NOT the same as "it failed"; keep waiting
		}
		// Trim the QUOTES as well as the whitespace. The jq filter ends in `// ""`,
		// so "no failed run" can come back as a literal two-character `""` — and
		// treating that as a URL would abort a perfectly healthy build on the
		// emptiness it was meant to signal. Only a real http(s) URL counts.
		u := strings.Trim(strings.TrimSpace(string(out)), `"`)
		if !strings.HasPrefix(u, "http") {
			return "", false
		}
		return u, true
	}
	// pinTriggerBuild kicks off the Build Container Images workflow on ref, passing
	// the exact sha to build via the `sha` input — `gh workflow run --ref` can only
	// name a branch/tag, so it would otherwise build the ref HEAD, which races a
	// concurrent push and produces a sha-<HEAD> tag the wait below never matches.
	// Needs an actions:write token.
	pinTriggerBuild = func(token, templateRepo, ref, sha string) error {
		cmd := exec.Command("gh", "workflow", "run", "build-images.yml",
			"--repo", templateRepo, "--ref", ref, "-f", "image=all", "-f", "sha="+sha)
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
		return cmd.Run()
	}
)

func Max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

type PinImagesOpts struct {
	Instance, Owner, TemplateRepo, SHA, Ref, Actor string
	TemplateToken, InstanceToken                   string
	Interval                                       time.Duration
	Retries                                        int
	BuildIfMissing                                 bool
	TriggerOnly                                    bool
}

func RunPinInstanceImages(o PinImagesOpts) error {
	required := []struct{ name, val string }{
		{"--instance", o.Instance}, {"--owner", o.Owner}, {"--template-repo", o.TemplateRepo},
		{"--sha", o.SHA}, {"GH_TOKEN_TEMPLATE", o.TemplateToken}, {"GH_TOKEN_INSTANCE", o.InstanceToken},
	}
	if o.TriggerOnly {
		// trigger-only never touches the instance's variables, so the instance
		// credential is not needed (the later full invocation validates it).
		required = required[:len(required)-1]
	}
	for _, v := range required {
		if v.val == "" {
			return fmt.Errorf("pin-instance-images: %s is required", v.name)
		}
	}
	if err := pinDockerLogin(o.TemplateToken, o.Actor); err != nil {
		return fmt.Errorf("docker login ghcr.io failed: %w", err)
	}

	// --build-if-missing intends to (re)build THIS commit's image, so it needs a ref.
	if o.BuildIfMissing && o.Ref == "" {
		return fmt.Errorf("pin-instance-images: --ref is required with --build-if-missing (the branch/tag to build)")
	}

	built, err := commitBuiltImages(o.TemplateToken, o.TemplateRepo, o.SHA)
	if err != nil {
		return err
	}

	// Pin THIS commit's sha-tagged images (their baked llz matches the rendered
	// workflow — assert-image-fresh compares the sha) whenever a build ran for the
	// commit (release/main) OR we're allowed to build one (--build-if-missing — the
	// branch/dispatch e2e path, where build-images never auto-ran, so there is NO
	// sha image and :latest is stale). When we want the sha image but it's absent,
	// trigger a build unless one is already in flight — covering both a failed/
	// mid-flight build on main AND a fresh branch build. One build covers every
	// pinImages entry.
	wantSha := built || o.BuildIfMissing
	if o.BuildIfMissing && anyShaImageMissing(o.Owner, o.SHA) {
		if pinBuildInProgress(o.TemplateToken, o.TemplateRepo, o.SHA) {
			fmt.Printf("Build Container Images already running for %.8s — waiting for it to publish.\n", o.SHA)
		} else {
			fmt.Printf("Images for %.8s are missing and no build is in progress — triggering Build Container Images on %s.\n", o.SHA, o.Ref)
			if err := pinTriggerBuild(o.TemplateToken, o.TemplateRepo, o.Ref, o.SHA); err != nil {
				return fmt.Errorf("could not trigger Build Container Images on %s — GH_TOKEN_TEMPLATE needs actions:write: %w", o.Ref, err)
			}
		}
	}
	// trigger-only: the build (if any was needed) is now in flight — return so the
	// caller's other work (scaffold/render/push in release-e2e's instantiate)
	// overlaps the publish instead of serializing behind it. The later full
	// invocation finds the run in progress and does the wait + pin.
	if o.TriggerOnly {
		fmt.Println("trigger-only: not waiting or pinning — a later full pin-instance-images run completes the pin.")
		return nil
	}

	for _, im := range pinImages {
		base := fmt.Sprintf("ghcr.io/%s/%s", o.Owner, im.Name)
		ref := imageRef(base, o.SHA, wantSha)
		if wantSha {
			fmt.Printf("Waiting for %s to publish…\n", ref)
			ok, failedURL := waitForManifest(ref, o.TemplateToken, o.TemplateRepo, o.SHA, o.Retries, o.Interval)
			if !ok {
				if failedURL != "" {
					return fmt.Errorf("%s will never publish: Build Container Images FAILED for %.8s — %s "+
						"(aborting the wait instead of polling out the budget)", ref, o.SHA, failedURL)
				}
				return fmt.Errorf("%s not published in time and no failed build run was found for %.8s — "+
					"the build may still be queued, or the run was never triggered", ref, o.SHA)
			}
		} else if !pinManifestExists(ref) {
			return fmt.Errorf("%s not found in GHCR", ref)
		}
		if _, err := pinGHRetry(o.InstanceToken, "variable", "set", im.Var, "--repo", o.Instance, "--body", ref); err != nil {
			return fmt.Errorf("could not set %s on %s — GH_TOKEN_INSTANCE needs 'Variables: read and write': %w", im.Var, o.Instance, err)
		}
		fmt.Printf("Pinned %s %s=%s\n", o.Instance, im.Var, ref)
	}
	return waitForRequiredImages(o, wantSha)
}

// waitForRequiredImages blocks until every image the CLUSTER will pull for this
// commit is published. Nothing pins a variable at these, so the loop above never
// looked at them — see requiredShaImages for the release-e2e round that cost.
func waitForRequiredImages(o PinImagesOpts, wantSha bool) error {
	if !wantSha {
		// Pinning :latest — the cluster is rendered against a moving tag that
		// already exists, so there is nothing commit-specific to wait for.
		return nil
	}
	for _, name := range requiredShaImages {
		ref := fmt.Sprintf("ghcr.io/%s/%s:sha-%s", o.Owner, name, o.SHA)
		fmt.Printf("Waiting for %s to publish (consumed in-cluster; Kyverno verifies it at admission)…\n", ref)
		ok, failedURL := waitForManifest(ref, o.TemplateToken, o.TemplateRepo, o.SHA, o.Retries, o.Interval)
		if ok {
			continue
		}
		hint := "the build may still be queued, or the run was never triggered"
		if failedURL != "" {
			hint = "Build Container Images FAILED for this commit — " + failedURL
		}
		return fmt.Errorf("%s is not published, and the cluster is about to be rendered against it: %s.\n"+
			"Kyverno's verify-llz-image-signature policy will refuse every workload that references it "+
			"(llz-reconciler, obj-proxy, harbor-robot-provisioner, broad-pat-rotator), and convergence will "+
			"then wait out its whole budget on the downstream symptom — a missing apl-overlay push — rather "+
			"than on this", ref, hint)
	}
	return nil
}

// anyShaImageMissing reports whether any pinned image's sha-<sha> tag is not yet
// published — the signal that a build is incomplete or failed.
func anyShaImageMissing(owner, sha string) bool {
	for _, name := range shaImageNames() {
		if !pinManifestExists(fmt.Sprintf("ghcr.io/%s/%s:sha-%s", owner, name, sha)) {
			return true
		}
	}
	return false
}

// shaImageNames is every image this commit must have published: the ones a
// variable is pinned at, plus the ones only the cluster consumes.
func shaImageNames() []string {
	names := make([]string, 0, len(pinImages)+len(requiredShaImages))
	for _, im := range pinImages {
		names = append(names, im.Name)
	}
	return append(names, requiredShaImages...)
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
// (i.e. the commit touched tools/ or dockerfiles/, so a per-commit image is/was built).
func commitBuiltImages(token, templateRepo, sha string) (bool, error) {
	out, err := pinGHRetry(token, "api",
		fmt.Sprintf("repos/%s/actions/runs?head_sha=%s&per_page=100", templateRepo, sha),
		"--jq", `[.workflow_runs[] | select(.name=="Build Container Images")] | length`)
	if err != nil {
		return false, fmt.Errorf("querying Build Container Images runs for %.8s: %w", sha, err)
	}
	return parseBuildCount(out) > 0, nil
}

// pinGHRetry wraps pinGH with a short retry (3 attempts, 5s/10s backoff via the
// seamed pinSleep) for the FATAL gh calls on the pin path. A single transient
// GitHub API 503 on the very first Instantiate query has killed a whole
// release-e2e dispatch at minute one, during a live API incident; a couple of
// retries ride that out. Persistent failures still
// surface the final error unchanged.
func pinGHRetry(token string, args ...string) (out []byte, err error) {
	for attempt := 1; ; attempt++ {
		out, err = pinGH(token, args...)
		if err == nil || attempt >= 3 {
			return out, err
		}
		fmt.Fprintf(os.Stderr, "::warning::gh %s failed (attempt %d/3): %v — retrying\n", args[0], attempt, err)
		pinSleep(time.Duration(attempt) * 5 * time.Second)
	}
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

// waitForManifest polls pinManifestExists until the image is published, the build
// that would publish it is known to have FAILED, or the retry budget is spent; the
// first check is immediate. Returns whether it appeared, and the failed run's URL
// when it aborted early.
//
// The abort is what turns a 20-minute dead wait into an immediate, named failure.
// The image check stays FIRST: a build can fail after a successful push (a later
// image in the matrix), and in that case the artifact we need is already there.
func waitForManifest(image, token, templateRepo, sha string, retries int, delay time.Duration) (bool, string) {
	for attempt := 0; ; attempt++ {
		if pinManifestExists(image) {
			return true, ""
		}
		if url, failed := pinBuildFailed(token, templateRepo, sha); failed {
			return false, url
		}
		if attempt >= retries {
			return false, ""
		}
		pinSleep(delay)
	}
}
