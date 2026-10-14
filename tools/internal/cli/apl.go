package cli

// STAYS IN PACKAGE MAIN: it builds the `llz apl` SUBTREE, wiring five sibling
// command groups together. That is the tree, and main owns the tree — the rule
// is "an extension owns its own command", not "every constructor leaves".
//
import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/reachability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/identityconfig"
	openbaoext "github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/openbao"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/onboard"
	"github.com/spf13/cobra"
)

// aplCmd is the APL-layer front door (ADR 0013 — "one binary, two altitudes"): a
// noun-verb subtree that speaks App Platform's domain model. `user` is HOMED here
// (its top-level `llz users` alias was retired), `values` groups the apl-values
// commands, and `app` lists + toggles App Platform apps; `openbao`/`status`/
// `doctor`/`verify` still DELEGATE to their existing top-level command, re-labeled
// to App Platform vocabulary. The tree grows (`apl values set/show`, `apl team`)
// in later phases. See ADR 0013 Appendix A/B.
//
// Secrets are deliberately NOT unified under an `apl secret` verb: the two stores
// are distinct backends and stay distinct (ADR 0013 Appendix B). The platform
// runtime secret store — OpenBao KV — is surfaced here as `apl openbao`; GitHub
// build-time secrets remain `llz secrets` (provider/CI plumbing), out of `apl`.
func aplCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "apl",
		Short: "App Platform layer: users, apps, values, secrets & platform health (ADR 0013)",
		Long: "The APL-layer front door (ADR 0013, \"one binary, two altitudes\").\n\n" +
			"`apl user` onboards platform users, `apl values` renders/validates the\n" +
			"App Platform values, and `apl openbao` reaches the platform secret store;\n" +
			"the remaining leaves delegate to their existing top-level equivalents and\n" +
			"move down into internal/apl in later phases.",
	}
	c.AddCommand(
		identityconfig.AplUserCmd(), // apl user — onboarding (retired from the top level)
		aplAppCmd(),                 // apl app — list | enable | disable App Platform apps
		aplValuesCmd(),              // apl values — render the apl-values/overlay tree
		openbaoext.OpenbaoCmd(),     // apl openbao — platform secret store (OpenBao KV); GitHub secrets stay `llz secrets`
		statusCmd(),                 // apl status — platform health
		onboard.DoctorCmd(),         // apl doctor — APL-scoped readiness
		reachability.VerifyCmd(),    // apl verify — platform verification
	)
	return c
}

// aplValuesCmd is `llz apl values` — author the App Platform values (apl-values):
// `render` reconciles the LandingZone spec into the values/overlay tree. The
// ADR's target grows `set` and `show` here in later phases (Appendix A).
//
// IT NO LONGER HAS A `validate` LEAF. That leaf surfaced `llz ci
// validate-apl-values`, whose two checks — the runtime-placeholder var-contract
// and apl-core's chart schema — both took a rendered apl-core values.yaml as
// input. On the managed App Platform LLZ renders no such file (Linode owns
// apl-core's values; render_test and scaffold-render-check both assert LLZ never
// emits one), so the verb had no input left to validate and nothing had called it
// since the pivot. What LLZ does render — the apl-overlay — is checked by
// `llz render --check` and the overlay's own tests.
func aplValuesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "values",
		Short: "author the App Platform values (apl-values): render",
	}
	c.AddCommand(
		render.RenderCmd(), // apl values render — spec → values/overlay tree
	)
	return c
}

// renamed re-labels a freshly constructed command's verb and one-line help so
// the APL subtree can present App Platform vocabulary over an existing
// implementation without duplicating it. It mutates a copy returned by the
// constructor, so the original top-level command is unaffected.
func renamed(c *cobra.Command, use, short string) *cobra.Command {
	c.Use, c.Short = use, short
	return c
}
