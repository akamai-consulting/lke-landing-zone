package releasepublish

// cobra_releasepublish.go — the `llz ci publish-charts`, `pin-instance-images` and
// `assert-instance-pr-gates` flag sets.
//
// The verbs are tools/internal/extensions/lifecycle/releasepublish, which declares the extension — and
// declares that only one of the three has no lifecycle state to attach to.

import (
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func PublishChartsCmd() *cobra.Command {
	var o PublishChartsOpts
	var interval int
	c := &cobra.Command{
		Use:   "publish-charts",
		Short: "package, push, and keyless-sign first-party charts to an OCI registry (immutable + re-sign)",
		Long: "Packages every chart under --dir and pushes + cosign-signs it to\n" +
			"oci://<registry>/<owner>/<repo-path>/<chart>. Immutable: a version already\n" +
			"published AND signed is skipped; a version pushed but UNSIGNED (an earlier\n" +
			"run whose sign failed) is re-signed in place. Transient helm/cosign failures\n" +
			"retry. Replaces the publish-charts workflow's inline bash with tested Go.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			o.Interval = time.Duration(interval) * time.Second
			return RunPublishCharts(o)
		},
	}
	c.Flags().StringVar(&o.ChartsDir, "dir", "kubernetes-charts", "directory holding the chart subdirectories")
	c.Flags().StringVar(&o.Selected, "selected", "all", "chart name to publish, or \"all\"")
	c.Flags().StringVar(&o.Registry, "registry", "ghcr.io", "OCI registry host")
	c.Flags().StringVar(&o.Owner, "owner", "", "registry namespace owner (lowercased org)")
	c.Flags().StringVar(&o.RepoPath, "repo-path", "charts", "repository path prefix under the owner")
	c.Flags().StringVar(&o.DestDir, "dest", "/tmp/charts", "directory for packaged .tgz files")
	c.Flags().IntVar(&o.Retries, "retries", 5, "attempts for each flaky helm push / cosign step")
	c.Flags().IntVar(&interval, "interval", 10, "seconds between retries")
	return c
}

// runCapture runs a command and, on failure, folds its combined output into the
// error — so a cosign/helm "exit status 1" is actually debuggable in CI logs
// (exec.Command(...).Run() otherwise discards stderr).
func PinInstanceImagesCmd() *cobra.Command {
	var instance, owner, templateRepo, sha, ref string
	var interval, timeout int
	var buildIfMissing, triggerOnly bool
	c := &cobra.Command{
		Use:   "pin-instance-images",
		Short: "pin the e2e instance's TF_IMAGE/KUBE_IMAGE to this commit's ci images",
		Long: "Points the instance repo's TF_IMAGE / KUBE_IMAGE variables at the ci-tofu\n" +
			"/ ci-kubernetes images for --sha, so the baked llz binary can't drift from the\n" +
			"rendered workflow. If this commit triggered a Build Container Images run, waits\n" +
			"for its sha- image to publish and pins the exact sha; otherwise pins :latest\n" +
			"(the binary is unchanged). Reads GH_TOKEN_TEMPLATE (this repo's runs + GHCR\n" +
			"reads) and GH_TOKEN_INSTANCE (instance variable writes) from the environment.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunPinInstanceImages(PinImagesOpts{
				Instance: instance, Owner: strings.ToLower(owner), TemplateRepo: templateRepo,
				SHA: sha, Ref: ref, Actor: os.Getenv("GITHUB_ACTOR"),
				TemplateToken: os.Getenv("GH_TOKEN_TEMPLATE"), InstanceToken: os.Getenv("GH_TOKEN_INSTANCE"),
				Interval:       time.Duration(interval) * time.Second,
				Retries:        timeout / Max1(interval),
				BuildIfMissing: buildIfMissing,
				TriggerOnly:    triggerOnly,
			})
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "instance repo owner/name (TF_IMAGE/KUBE_IMAGE are set here)")
	c.Flags().StringVar(&owner, "owner", "", "GHCR namespace owner (this repo's org)")
	c.Flags().StringVar(&templateRepo, "template-repo", "", "this (template) repo owner/name — queried for the build run")
	c.Flags().StringVar(&sha, "sha", "", "the commit whose images to pin")
	c.Flags().StringVar(&ref, "ref", "", "branch/tag to (re)trigger Build Container Images on with --build-if-missing (its HEAD must be --sha)")
	c.Flags().BoolVar(&buildIfMissing, "build-if-missing", false, "if this commit's sha images are missing (a failed/incomplete build, OR a branch where build-images never auto-ran), trigger Build Container Images on --ref, wait, and pin the sha — instead of pinning a stale :latest or failing")
	c.Flags().BoolVar(&triggerOnly, "trigger-only", false, "with --build-if-missing: trigger a missing build and return WITHOUT waiting or pinning, so the publish wait overlaps the caller's other work; a later full invocation finds the build in flight and pins")
	c.Flags().IntVar(&interval, "interval", 20, "seconds between manifest polls while waiting for a sha image")
	c.Flags().IntVar(&timeout, "timeout", 1200, "max seconds to wait for a just-built sha image to publish")
	return c
}

// AssertInstancePRGatesCmd is the e2e proof that the DELIVERED pull_request-gated CI
// actually runs. It sits next to pin-instance-images because it is the same lane —
// e2e-instantiate.yml drives the same throwaway instance repo, and this step runs
// immediately after the pin so the gates execute in THIS commit's image.
func AssertInstancePRGatesCmd() *cobra.Command {
	var o PRGatesOpts
	var interval, timeout int
	c := &cobra.Command{
		Use:   "assert-instance-pr-gates",
		Short: "prove the instance's pull_request-gated CI (tf-lint + checkov) actually runs and passes",
		Long: "Opens a throwaway PR on the instance repo touching a path the terraform\n" +
			"pipeline's paths: filter watches, waits for the delivered pull_request-gated\n" +
			"checks, and fails if they did not appear or did not pass — then closes the PR\n" +
			"and deletes the branch. Those jobs are gated on `pull_request`, and the fixture\n" +
			"repo is driven by force-push/dispatch/schedule, so nothing had ever triggered\n" +
			"them: they shipped broken for several releases. Reads GH_TOKEN from the\n" +
			"environment (contents:write + pull-requests:write on the instance repo).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			o.Token = os.Getenv("GH_TOKEN")
			o.Interval = time.Duration(interval) * time.Second
			o.Retries = timeout / Max1(interval)
			return RunAssertInstancePRGates(o)
		},
	}
	c.Flags().StringVar(&o.Instance, "instance", "", "instance repo owner/name to open the throwaway PR on")
	c.Flags().StringVar(&o.SHA, "sha", "", "the template commit under test — names the throwaway branch")
	c.Flags().StringVar(&o.Host, "host", "github.com", "forge host for the clone URL (the GHES appliance on that lane)")
	c.Flags().StringVar(&o.TouchPath, "touch-path", DefaultPRGateTouchPath, "repo-relative file to touch so the paths: filter selects the gated jobs")
	c.Flags().StringSliceVar(&o.Checks, "check", DefaultPRGateChecks, "check name that must appear AND succeed (repeatable)")
	c.Flags().BoolVar(&o.Keep, "keep", false, "leave the branch and PR behind instead of closing them, to inspect a failure by hand")
	c.Flags().IntVar(&interval, "interval", 20, "seconds between check polls")
	c.Flags().IntVar(&timeout, "timeout", 1200, "max seconds to wait for the gated checks to appear and settle")
	return c
}
