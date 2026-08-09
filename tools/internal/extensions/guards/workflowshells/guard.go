package workflowshells

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
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

func Run(dir string) error {
	repo, rel := capability.RepoContaining(workflowShellsBinding(), dir)
	entries, err := repo.ReadDir(rel)
	if err != nil {
		return fmt.Errorf("check-workflow-shells: %w", err)
	}
	var violations []string
	var examined int
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		data, err := repo.ReadFile(filepath.Join(rel, e.Name()))
		if err != nil {
			return fmt.Errorf("check-workflow-shells: %w", err)
		}
		examined++
		violations = append(violations, scanWorkflowShells(e.Name(), data)...)
	}
	sort.Strings(violations)
	for _, v := range violations {
		fmt.Printf("::error::%s\n", v)
	}
	if len(violations) > 0 {
		return fmt.Errorf("check-workflow-shells: %d container job(s) can fall back to /bin/sh — add `defaults:\\n  run:\\n    shell: bash` (a `set -o pipefail` step otherwise runs under dash and fails)", len(violations))
	}
	// REFUSE AN EMPTY CORPUS, the rule plaintext-guard states and this guard was
	// the last driven one not keeping: "a guard that had nothing to check reports
	// the same green as one that checked everything, so this fails instead."
	//
	// It matters most HERE, because the subject is a directory passed in. Pointed
	// at a path that does not hold workflows — a moved .github/, a wrong --dir, a
	// driver arg carried over from a different tree — this printed "every container
	// job declares a bash shell default" over zero jobs.
	if examined == 0 {
		return fmt.Errorf("check-workflow-shells: no workflow YAML under %q — a guard that "+
			"examined nothing cannot report that everything is fine. Check --dir", dir)
	}
	fmt.Printf("check-workflow-shells: every container job declares a bash shell default (%d workflow(s)).\n", examined)
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
