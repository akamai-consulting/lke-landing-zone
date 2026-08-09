package envtopology

// cobra_list.go — the CLI surface for list.
//
// Split from list.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func ListCmd() *cobra.Command {
	var jsonOut, haOnly, ordered bool
	var role string
	c := &cobra.Command{
		Use:   "list",
		Short: "list the scaffolded deployments (the CI matrix source of truth)",
		Long: "Lists every topo.Deployment scaffolded by `llz env add`, from the UNION of two\n" +
			"sources: the LandingZone spec's environments/<name>.yaml (the source of\n" +
			"truth you commit) and any terraform-iac-bootstrap/cluster/<name>.tfvars.\n" +
			"The union matters because the tfvars are gitignored build artifacts —\n" +
			"on a fresh clone the spec is the only source, so a spec-only topo.Deployment\n" +
			"must still appear. The CI workflows' `discover` job runs\n" +
			"`llz env list --json` and feeds it into each per-topo.Deployment matrix, so a\n" +
			"new topo.Deployment is covered everywhere the moment it is added. --ha narrows to the\n" +
			"OpenBao HA members (ha_role != standalone); --role filters by exact role.\n" +
			"--ordered emits only the deployments that declare a promotion_rank, in\n" +
			"ascending promotion order (dev → staging → prod) — the sequence a\n" +
			"promote-on-color.Green pipeline walks (see `llz env next`).\n" +
			"Layout-aware (instance root or a template-repo checkout).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runEnvList(jsonOut, haOnly, ordered, role) },
	}
	f := c.Flags()
	f.BoolVar(&jsonOut, "json", false, "emit a JSON array of topo.Deployment names (for `fromJSON` in a workflow matrix)")
	f.BoolVar(&haOnly, "ha", false, "only deployments in an HA pair (ha_role active|standby)")
	f.BoolVar(&ordered, "ordered", false, "only ranked deployments, in promotion order (ascending promotion_rank)")
	f.StringVar(&role, "role", "", "only deployments with this exact ha_role (active|standby|standalone)")
	return c
}
