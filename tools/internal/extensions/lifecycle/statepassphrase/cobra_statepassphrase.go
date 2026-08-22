package statepassphrase

// cobra_statepassphrase.go — the `llz ci rotate-state-passphrase` flag set, and the
// Deps wiring for tools/internal/extensions/lifecycle/statepassphrase.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghapi"
	"github.com/spf13/cobra"
)

func Init() {
	Install(Deps{
		Exec:        execOutput,
		GHJSONPaged: ghapi.GHAPIJSONPaged,
	})
}

func RotateStatePassphraseCmd() *cobra.Command {
	var apply bool
	var rootsDir string
	c := &cobra.Command{
		Use:   "rotate-state-passphrase",
		Short: "re-key every Terraform root's state onto the new encryption passphrase",
		Long: "Rollover half of state-encryption key rotation. Requires a rotation window:\n" +
			"TF_ENCRYPTION must already carry BOTH key providers (terraform-Init emits the\n" +
			"encrypted fallback when TF_STATE_ENCRYPTION_PASSPHRASE_OLD is set), and\n" +
			"the new passphrase reaches it via TF_STATE_ENCRYPTION_PASSPHRASE.\n\n" +
			"Per root: `tofu state pull | tofu state push -` re-encrypts under the new\n" +
			"primary (no provider calls, plaintext never on disk), then the state is read\n" +
			"back with the NEW KEY ALONE. Only when EVERY root verifies is it safe to\n" +
			"delete TF_STATE_ENCRYPTION_PASSPHRASE_OLD — until then the old passphrase is\n" +
			"the only way to read any root that did not roll over.\n\n" +
			"Idempotent: a root already on the new key verifies and is left alone, so a\n" +
			"partial run is resumed by re-running.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return RunRotate(apply, rootsDir)
		},
	}
	c.Flags().BoolVar(&apply, "apply", false, "perform the rollover (default: report what would be re-keyed)")
	// `terraform-iac-bootstrap`, NOT `terraform`. The roots have never lived at
	// the old path — `llz render` materialises them into
	// terraform-iac-bootstrap/<root>, which is also where the terraform-init
	// action cds. The wrong default did not error: every root simply failed its
	// os.Stat and was recorded as "not present in this instance", and an
	// all-skipped rollover then reported success. See RunRotate.
	c.Flags().StringVar(&rootsDir, "roots-dir", "terraform-iac-bootstrap", "directory holding the Terraform roots")
	return c
}
