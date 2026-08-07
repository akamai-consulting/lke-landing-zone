package assertobs

// cobra_readiness.go — the CLI surface for readiness.
//
// Split from readiness.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"os"
	"time"

	"github.com/spf13/cobra"
)

func AssertLokiCmd() *cobra.Command {
	var nameMatch, region string
	var settle, interval int
	var noFlushProbe bool
	c := &cobra.Command{
		Use:   "assert-loki",
		Short: "fail unless Loki is bootstrapped (workloads Ready + S3-backed) on the current cluster",
		Long: "Native port of assert-loki-bootstrapped.sh. Asserts Loki's workloads are Ready\n" +
			"AND its config references S3 object storage (the kyverno loki-s3-object-store\n" +
			"policy mutates object_store filesystem→s3 — \"s3-backed\" is the real signal log\n" +
			"persistence works). Best-effort reports the Loki Argo Application status\n" +
			"(non-gating). Polls for a short settle budget so a transient kubectl/apiserver\n" +
			"blip (or a brief readiness / kyverno-mutation lag) doesn't flake the gate — the\n" +
			"same treatment assert-scrape-targets/assert-reconciler already carry. Exit 0\n" +
			"bootstrapped, 1 otherwise.\n\n" +
			"PROVES the write path rather than inferring it: if nothing has reached the chunks\n" +
			"bucket since the ingesters started, it POSTs /flush to each ingester and waits for a\n" +
			"chunk to land. Every other check here passes on a Loki that has never written a byte\n" +
			"(#397), and two observational attempts to catch that both passed vacuously on a real\n" +
			"cluster. --no-flush-probe keeps the gate strictly read-only at the cost of the proof.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertLoki(nameMatch, region, time.Duration(settle)*time.Second, time.Duration(interval)*time.Second, !noFlushProbe)
		},
	}
	c.Flags().StringVar(&nameMatch, "name-match", "loki", "substring/regex identifying Loki workloads/objects")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling for Loki to bootstrap before failing (rides out a transient kubectl blip / readiness lag)")
	c.Flags().IntVar(&interval, "interval", 10, "seconds between poll attempts")
	c.Flags().StringVar(&region, "region", "",
		"deployment whose spec names the Loki chunks bucket the write proof reads. Threaded in rather than "+
			"read from $REGION here, for the reason assertSuiteLanes documents: a call site that reaches for "+
			"the environment turns a missing value into a silent skip instead of a visible one")
	c.Flags().BoolVar(&noFlushProbe, "no-flush-probe", false,
		"do not POST /flush to the ingesters to force a write. The lane then reports UNPROVEN instead of "+
			"proving the write path, because nothing else on a young cluster makes Loki write on demand")
	return c
}
func WaitHarborCmd() *cobra.Command {
	var harborURL string
	var registryOnly bool
	c := &cobra.Command{
		Use:   "wait-harbor",
		Short: "wait for the harbor-registry rollout (the post-S3-seed gate)",
		Long: "Waits for harbor-registry to roll out. It mounts the harbor-registry-s3\n" +
			"Secret via secretKeyRef, so it stays in CreateContainerConfigError until that\n" +
			"Secret exists — seeded mid-bootstrap, then synced when the es-store-recovery\n" +
			"lane sees the store go Ready. Exit 0 rolled out, 1 on timeout.\n\n" +
			"This verb used to carry a second, PRE-seed half (admin Secret + control-plane\n" +
			"Deployments/StatefulSets + an API ping). That half gated the workflow's\n" +
			"`harbor` job, whose robot provisioning moved in-cluster in f0aa68f; the job\n" +
			"went with it and took the gate's only caller, leaving the code unreachable.\n" +
			"kick-harbor-provisioner now does its own harbor-core Available wait.\n\n" +
			"--registry-only and --harbor-url are accepted and IGNORED. Instance repos\n" +
			"vendor their workflows, so a rendered-but-not-yet-upgraded instance can still\n" +
			"pass --registry-only; rejecting it would break those instances on image bump\n" +
			"alone. They go once `llz upgrade` has carried the new call site everywhere.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIWaitHarbor(harborURL, registryOnly)
		},
	}
	c.Flags().StringVar(&harborURL, "harbor-url", os.Getenv("HARBOR_URL"), "accepted and ignored (vendored-workflow compatibility)")
	c.Flags().BoolVar(&registryOnly, "registry-only", false, "accepted and ignored — the registry rollout is now the only behavior (vendored-workflow compatibility)")
	_ = c.Flags().MarkDeprecated("harbor-url", "it is ignored; the API ping was retired with the pre-seed gate")
	_ = c.Flags().MarkDeprecated("registry-only", "it is ignored; the registry rollout is now the only behavior")
	return c
}
func HarborTrustObjProxyCACmd() *cobra.Command {
	return &cobra.Command{
		Use:   "harbor-trust-obj-proxy-ca",
		Short: "roll harbor-registry if its pods predate the obj-proxy CA policy (no-op when objProxy is off)",
		Long: "Closes the admission race between apl-core's Harbor install and the Kyverno\n" +
			"policy that mounts the obj-proxy CA. The policy mutates on ADMISSION, so pods\n" +
			"that already existed when it landed carry no CA — and once the CoreDNS rewrite\n" +
			"is live those pods cannot complete a single S3 call, without crashing and\n" +
			"without reporting anything.\n\n" +
			"MUST RUN AFTER CONVERGE. It keys off the ClusterPolicy's existence to decide\n" +
			"whether objProxy is enabled here, and the policy arrives with the component's\n" +
			"Argo sync — so running it earlier (it used to be folded into wait-harbor) sees\n" +
			"no policy, concludes the component is off, and silently does nothing.\n\n" +
			"Always exits 0: a failure to repair is a ::warning::, and\n" +
			"`llz ci assert-obj-encryption` is the gate that fails.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			retrofitHarborObjProxyCA()
			return nil
		},
	}
}
