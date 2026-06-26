package main

// ci_workflow_shells.go implements `llz ci check-workflow-shells` — a CI guard
// that fails when a workflow job runs in a `container:` but its `run:` steps can
// fall back to the container's /bin/sh (dash).
//
// WHY: GitHub uses bash for `run:` steps on the host, but inside a container it
// falls back to `sh` when no bash default is declared. A `set -o pipefail` (a
// bashism the repo's steps use) then fails under dash with "Illegal option -o
// pipefail" — which is exactly how llz-discover-deployments.yml silently broke
// the scheduled auto-unseal / scheduled-checks / secret-rotation workflows every
// cycle until a `defaults.run.shell: bash` was added. This guard makes that class
// of regression fail at lint time instead of in production.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func ciCheckWorkflowShellsCmd() *cobra.Command {
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
		RunE: func(_ *cobra.Command, _ []string) error { return runCheckWorkflowShells(dir) },
	}
	c.Flags().StringVar(&dir, "dir", ".github/workflows", "directory of workflow YAML files to scan")
	return c
}

func runCheckWorkflowShells(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("check-workflow-shells: %w", err)
	}
	var violations []string
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("check-workflow-shells: %w", err)
		}
		violations = append(violations, scanWorkflowShells(e.Name(), data)...)
	}
	sort.Strings(violations)
	for _, v := range violations {
		fmt.Printf("::error::%s\n", v)
	}
	if len(violations) > 0 {
		return fmt.Errorf("check-workflow-shells: %d container job(s) can fall back to /bin/sh — add `defaults:\\n  run:\\n    shell: bash` (a `set -o pipefail` step otherwise runs under dash and fails)", len(violations))
	}
	fmt.Println("check-workflow-shells: every container job declares a bash shell default.")
	return nil
}

type wfShellDefaults struct {
	Run struct {
		Shell string `yaml:"shell"`
	} `yaml:"run"`
}

// scanWorkflowShells returns one finding per container job whose run-steps can
// fall back to dash. An unparseable file yields no findings — actionlint owns
// syntax; this guard only judges the shell-default invariant.
func scanWorkflowShells(file string, data []byte) []string {
	var wf struct {
		Defaults wfShellDefaults `yaml:"defaults"`
		Jobs     map[string]struct {
			Container interface{}     `yaml:"container"`
			Defaults  wfShellDefaults `yaml:"defaults"`
			Steps     []struct {
				Run   string `yaml:"run"`
				Shell string `yaml:"shell"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil
	}
	wfBash := isBashShell(wf.Defaults.Run.Shell)

	ids := make([]string, 0, len(wf.Jobs))
	for id := range wf.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var out []string
	for _, id := range ids {
		job := wf.Jobs[id]
		if job.Container == nil {
			continue // host runner → bash is the default
		}
		jobBash := wfBash || isBashShell(job.Defaults.Run.Shell)
		for _, s := range job.Steps {
			if s.Run == "" { // `uses:` steps are unaffected
				continue
			}
			if jobBash || isBashShell(s.Shell) {
				continue
			}
			out = append(out, fmt.Sprintf("%s: job %q runs in a container with a `run:` step but declares no bash shell default — it falls back to /bin/sh (dash); add `defaults.run.shell: bash`", file, id))
			break // one finding per job is enough
		}
	}
	return out
}

func isBashShell(shell string) bool {
	return strings.HasPrefix(strings.TrimSpace(shell), "bash")
}
