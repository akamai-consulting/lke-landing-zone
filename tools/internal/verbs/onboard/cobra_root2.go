package onboard

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/spf13/cobra"
)

// cobra_root2.go — the `llz doctor` flag set.
//
// IT LANDED HERE, NOT IN internal/extensions/doctor, and the compiler decided
// that: this command calls RunDoctor, which is onboard's, and onboard already
// imports doctor. Putting the flag set in `doctor` closed the loop. The verb is
// named for the capability it REPORTS on; the code that runs it lives here.

// FallbackDoctorEnv is the deployment name to check when this checkout has no
// spec to name one — `e2e`, the TEMPLATE's own throwaway lane.
const FallbackDoctorEnv = "e2e"

// DefaultDoctorEnv is the deployment `llz doctor` reports on when --env is not
// given: the FIRST deployment in this instance's own spec, and only `e2e` when
// there is no spec to ask.
//
// IT USED TO BE THE BARE CONSTANT "e2e", WHICH IS A NAME NO ADOPTER HAS. `e2e` is
// the deployment the template's own release lane scaffolds; a real instance has
// `prod`, `primary`, `staging`. Measured against a live adopter mid-upgrade, the
// constant produced a readiness report headed `infra-e2e`, remediation reading
// `llz tokens --env e2e --yes`, and a closing error telling the operator to
// `llz env add e2e` — three wrong instructions wrapped around one correct
// finding (a genuinely missing TF_STATE_ENCRYPTION_PASSPHRASE), which is how a
// correct warning gets read as a broken tool and dismissed.
//
// RESOLVED LAZILY, in RunE rather than as the flag's construction-time default:
// every `llz` invocation builds the whole command tree, and a default that
// stat'ed the tree and parsed YAML would put that cost on `llz --help`.
func DefaultDoctorEnv() string {
	tfDir, _, _ := instancelayout.Detect()
	root := filepath.Dir(tfDir)
	lz, err := clusterspec.LoadInstance(root)
	if err == nil {
		if names := lz.EnvNames(); len(names) > 0 {
			return names[0] // EnvNames is sorted, so this is stable across runs
		}
	}
	// A SPEC THAT EXISTS BUT WILL NOT LOAD MUST NOT FALL BACK IN SILENCE. The
	// fallback is right for a checkout that HAS no spec; for one whose spec is
	// unreadable it silently reproduces the wrong-deployment advice this function
	// exists to remove — and the likeliest moment for an unparseable spec is
	// immediately after a `copier update`, which is exactly when `llz upgrade`
	// calls this. Saying so costs one line and turns a confusing report into a
	// diagnosable one.
	if clusterspec.InstancePresent(root) {
		// BOTH WAYS A SPEC CAN YIELD NO DEPLOYMENT, because the silent one is the
		// likelier. A spec that fails to PARSE is loud and rare (a bad merge). A
		// spec that loads with ZERO environments is the normal state of a fresh
		// `llz new` before the first `llz env add` — and falling back there
		// reproduces the whole infra-e2e / `llz tokens --env e2e` / "run llz env
		// add e2e" misdirection this function exists to remove, for the operator
		// least able to tell it is wrong.
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "%s this instance has a %s that could not be read (%v) — reporting on the "+
				"%q deployment instead, which is almost certainly not yours. Fix the spec, or pass --env.\n",
				color.Yellow("!"), clusterspec.LandingZoneFile, err, FallbackDoctorEnv)
		default:
			fmt.Fprintf(os.Stderr, "%s this instance has a %s but no deployments yet — reporting on %q, which "+
				"is the template's own lane and not yours. Add one with `llz env add <name>`, or pass --env.\n",
				color.Yellow("!"), clusterspec.LandingZoneFile, FallbackDoctorEnv)
		}
	}
	return FallbackDoctorEnv
}

func DoctorCmd() *cobra.Command {
	var repo, env, sshHost, knownHosts string
	var admin bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "am I ready to build? tooling + gh auth + deployment readiness + repo config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			explicit := cmd.Flags().Changed("env")
			if !explicit {
				env = DefaultDoctorEnv()
			}
			return RunDoctor(repo, env, admin, explicit, sshHost, knownHosts)
		},
	}
	c.Flags().StringVar(&repo, "repo", "", "instance repo for the readiness check (default: .copier-answers.yml, or example repo in --admin)")
	c.Flags().StringVar(&env, "env", FallbackDoctorEnv, "deployment env to check readiness for (default: this instance's first deployment; scans tfvars + overlay, then the repo config)")
	c.Flags().BoolVar(&admin, "admin", false, "also check the template repo's e2e harness")
	c.Flags().StringVar(&sshHost, "ssh-host", "", "also check port-22 reachability + host keys for this SSH host (e.g. a self-hosted Git host)")
	c.Flags().StringVar(&knownHosts, "known-hosts", "", "with --ssh-host: diff live keys against this committed known_hosts file")
	return c
}
