package main

import "github.com/spf13/cobra"

// aplCmd is the APL-layer front door (ADR 0002 — "one binary, two altitudes"): a
// noun-verb subtree that speaks App Platform's domain model. `user` is HOMED here
// — its top-level `llz users` alias was retired (ADR 0002 Appendix B). The other
// leaves still DELEGATE to their existing top-level command, re-labeled to App
// Platform vocabulary (components→app); those implementations move down into
// internal/apl, and the tree grows (`apl values`, `apl secret`, `apl team`), in
// later phases. See ADR 0002 Appendix A/B.
func aplCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "apl",
		Short: "App Platform layer: users, apps, values & platform health (ADR 0002)",
		Long: "The APL-layer front door (ADR 0002, \"one binary, two altitudes\").\n\n" +
			"`apl user` is the sole home of user onboarding; the remaining leaves\n" +
			"delegate to their existing top-level equivalents and move down into\n" +
			"internal/apl in later phases.",
	}
	c.AddCommand(
		aplUserCmd(), // apl user — onboarding (retired from the top level)
		renamed(componentsCmd(), "app", "enable/disable App Platform apps (components)"),
		renderCmd(), // apl render — the values front door
		statusCmd(), // apl status — platform health
		doctorCmd(), // apl doctor — APL-scoped readiness
		verifyCmd(), // apl verify — platform verification
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
