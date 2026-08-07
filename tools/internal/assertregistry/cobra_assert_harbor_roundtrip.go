package assertregistry

// cobra_assert_harbor_roundtrip.go — the cobra surface for the `assert-registry`
// extension (internal/assertregistry).
//
// THERE IS NO Deps WIRING HERE, and this is the only extension of seventeen that
// needs none. Everything internal/assertregistry touches is stdlib or already
// behind internal/harborauth's own package vars. A Deps struct was drafted and
// deleted — see the note on assertregistry.Run.

import (
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/harborauth"
	"github.com/spf13/cobra"
)

func AssertHarborRoundTripCmd() *cobra.Command {
	var secretNS, secretName, repo, registry string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-harbor-roundtrip",
		Short: "fail unless a minted Harbor robot can authenticate for pull AND push",
		Long: "Performs the OCI distribution v2 auth handshake with the robot credential ESO\n" +
			"materialized from secret/harbor/robot: fetch the Bearer challenge, exchange the\n" +
			"robot's basic auth for a scoped token, verify the token actually CARRIES\n" +
			"pull+push access, then exercise both (tags/list, and open+cancel a blob upload\n" +
			"session).\n\n" +
			"Managed instances once rendered HARBOR_HOST as \"harbor.\" — non-empty, so it\n" +
			"defeated every empty-string guard including the systeminfo fallback — and every\n" +
			"push and pull 401'd. Every credential in the chain was valid; the HOST was\n" +
			"wrong. Nothing caught it because nothing ever USED the credential: the\n" +
			"provisioner asserted it had created a robot, not that the robot could log in.\n\n" +
			"Harbor's token service returns 200 with a valid JWT and an EMPTY access list\n" +
			"when the credential lacks the scope, so this asserts the granted access, not the\n" +
			"status code. Uploads no layers and creates no tags. Exit 0 / 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return Run(secretNS, secretName, registry, repo,
				time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&secretNS, "secret-namespace", harborauth.RobotSecretNS, "namespace of the robot credential Secret")
	c.Flags().StringVar(&secretName, "secret-name", harborauth.RobotSecretName, "name of the robot credential Secret")
	c.Flags().StringVar(&registry, "registry", "", "registry host override (default: the Secret's registry_host)")
	c.Flags().StringVar(&repo, "repo", ProbeRepo, "repository the pull+push scope is requested against")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling before failing")
	c.Flags().IntVar(&interval, "interval", 15, "seconds between poll attempts")
	return c
}
