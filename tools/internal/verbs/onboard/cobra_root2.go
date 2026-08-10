package onboard

import "github.com/spf13/cobra"

// cobra_root2.go — the `llz doctor` flag set.
//
// IT LANDED HERE, NOT IN internal/extensions/doctor, and the compiler decided
// that: this command calls RunDoctor, which is onboard's, and onboard already
// imports doctor. Putting the flag set in `doctor` closed the loop. The verb is
// named for the capability it REPORTS on; the code that runs it lives here.

func DoctorCmd() *cobra.Command {
	var repo, env, sshHost, knownHosts string
	var admin bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "am I ready to build? tooling + gh auth + deployment readiness + repo config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return RunDoctor(repo, env, admin, cmd.Flags().Changed("env"), sshHost, knownHosts)
		},
	}
	c.Flags().StringVar(&repo, "repo", "", "instance repo for the readiness check (default: .copier-answers.yml, or example repo in --admin)")
	c.Flags().StringVar(&env, "env", "e2e", "deployment env to check readiness for (scans tfvars + overlay, then the repo config)")
	c.Flags().BoolVar(&admin, "admin", false, "also check the template repo's e2e harness")
	c.Flags().StringVar(&sshHost, "ssh-host", "", "also check port-22 reachability + host keys for this SSH host (e.g. a self-hosted Git host)")
	c.Flags().StringVar(&knownHosts, "known-hosts", "", "with --ssh-host: diff live keys against this committed known_hosts file")
	return c
}
