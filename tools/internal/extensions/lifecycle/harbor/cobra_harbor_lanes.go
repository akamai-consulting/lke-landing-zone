package harbor

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// cobra_harbor_lanes.go — the cobra wiring for the two Harbor lanes. The lanes themselves
// are internal/harbor.
//
// Neither takes a flag: both read their inputs from the environment, because both
// run as in-cluster jobs where there is no command line to put them on. That is
// why this file is so short — the "flag set" is a Long string and an env read.

func SeedStandbyHarborRobotsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "seed-standby-harbor-robots",
		Short: "seed secret/harbor/{robot,pull-robot} on a standby peer from the active's published GitHub secrets",
		Long: "The standby half of Harbor robot provisioning. A standby peer has no\n" +
			"in-cluster Harbor, so it replicates the active's robot credentials from the\n" +
			"repo-level HARBOR_* GitHub secrets the active's in-cluster\n" +
			"harbor-robot-provisioner CronJob published (the EXISTING_* env). Each\n" +
			"not-ready state (active's secrets not published yet) is a step-summary note\n" +
			"+ clean exit so bootstrap can simply re-run. Env: HARBOR_URL,\n" +
			"EXISTING_{ROBOT,SECRET,PULL_ROBOT,PULL_SECRET}, OPENBAO_ROOT_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			harborURL := os.Getenv("HARBOR_URL")
			registryHost := strings.TrimPrefix(strings.TrimPrefix(harborURL, "http://"), "https://")
			return SeedStandbyRobots(registryHost)
		},
	}
}

func HarborProvisionerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "harbor-provisioner",
		Short: "in-cluster convergence loop: ensure Harbor project + robots, seed OpenBao, publish repo secrets",
		Long: "In-cluster replacement for the bootstrap workflow's harbor job. Ensures the\n" +
			"`platform` project and the ci-firewall-controller / pull-platform robots\n" +
			"exist, seeds secret/harbor/{robot,pull-robot} via a Kubernetes-auth OpenBao\n" +
			"role (no root token), publishes the repo-level HARBOR_* GitHub secrets the\n" +
			"standby bootstrap seeds from, and smoke-tests the seeded credentials.\n" +
			"Not-ready states exit 0 (the CronJob retries); a 401 smoke exits 1 —\n" +
			"delete the stale robot in Harbor UI and the next tick recreates it.",
		Args: cobra.NoArgs,
		// The sidecar shutdown is deliberately HERE and not inside
		// harbor.RunProvisioner: that function is also driven in-process by the
		// reconciler's harbor lane (reconcile.go), which is a long-lived pod that
		// must not shut anything down when one pass finishes.
		RunE: func(_ *cobra.Command, _ []string) error {
			defer ShutdownIstioSidecar()
			return RunProvisioner()
		},
	}
}
