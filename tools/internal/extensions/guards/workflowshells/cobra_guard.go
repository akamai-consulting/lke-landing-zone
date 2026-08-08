package workflowshells

// cobra_guard.go — the CLI surface for guard.
//
// Split from guard.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import "github.com/spf13/cobra"

func Cmd() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "check-workflow-shells",
		Short: "fail if a container-job workflow step can fall back to /bin/sh (missing bash shell default)",
		Long: "Scans the workflow YAML files in --dir. A job that runs in a `container:`\n" +
			"and has at least one `run:` step must resolve to bash — via a workflow- or\n" +
			"job-level `defaults.run.shell: bash`, or a per-step `shell:` — otherwise GitHub\n" +
			"falls back to the container's /bin/sh (dash) and a `set -o pipefail` fails the\n" +
			"job. Reports each offending job as ::error:: and exits non-zero.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return Run(dir) },
	}
	c.Flags().StringVar(&dir, "dir", ".github/workflows", "directory of workflow YAML files to scan")
	return c
}
