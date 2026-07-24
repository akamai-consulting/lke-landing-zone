package main

import "github.com/spf13/cobra"

// aplCmd is the Phase 0 skeleton of the APL-layer front door (ADR 0002 — "one
// binary, two altitudes"): a noun-verb subtree that speaks App Platform's domain
// model. It is ADDITIVE and no-behavior-change — every leaf DELEGATES to the
// existing top-level command implementation, which keeps working unchanged.
// Verb vocabulary is aligned here (users→user, components→app) purely by
// re-labeling the delegated command.
//
// Later phases move the implementations down into internal/apl and grow the
// tree — `apl values set/render/validate`, `apl secret`, `apl team`. See ADR
// 0002 Appendix A for the target command map and Appendix B for the per-command
// disposition.
func aplCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "apl",
		Short: "App Platform layer: users, apps, values & platform health (ADR 0002)",
		Long: "The APL-layer front door (ADR 0002, \"one binary, two altitudes\").\n\n" +
			"Phase 0 scaffolding: each subcommand delegates to its existing top-level\n" +
			"equivalent, which continues to work unchanged. The tree grows — and its\n" +
			"implementations move down into internal/apl — in later phases.",
	}
	c.AddCommand(
		renamed(usersCmd(), "user", "onboard & manage App Platform (Keycloak) users"),
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
