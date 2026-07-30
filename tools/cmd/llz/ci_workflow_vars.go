package main

// ci_workflow_vars.go implements `llz ci workflow-vars` — the accounting gate
// over the GitHub repo variables an instance's CI actually reads.
//
// state.go calls e2eRequirements "the single source of truth" for what an
// e2e-ready instance needs, and `llz doctor` reports against it. Nothing checked
// that it still describes the workflows. It had drifted: eight `vars.*` were read
// by shipped workflows and named in neither e2eRequirements nor any optional
// list, so "absent from the requirements" meant two different things at once —
// deliberately optional, or a required variable nobody declared.
//
// The second is the expensive one. `vars.TF_IMAGE` alone is read 44 times and
// only ONE of those carries a `|| fallback`; an undeclared required variable is
// never mentioned by doctor and surfaces as an empty container image or a blank
// tfvar deep inside a CI run, which is exactly the class of setup trap doctor
// exists to front-load.
//
// The gate is bidirectional, because both directions rot:
//   - consumed but unaccounted -> fail (doctor would never mention it)
//   - accounted but no longer consumed -> fail (operator toil for a dead knob)
//
// SECRETS ARE OUT OF SCOPE here. `secrets.*` resolution also involves
// `secrets: inherit` and environment scoping, so a name-only scan would report
// confidently and wrongly. Variables resolve lexically and can be checked honestly.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// workflowVarRoots are the trees whose workflows define the variable surface: the
// instance scaffold (what an adopter's repo needs) and the template repo's own CI
// (the e2e harness, covered by the --admin requirements).
var workflowVarRoots = []string{"instance-template/.github", ".github"}

var reWorkflowVar = regexp.MustCompile(`vars\.([A-Z_][A-Z0-9_]*)`)

// sortedNames is sortedKeys for any map value type (sortedKeys is
// map[string]string only).
func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func ciWorkflowVarsCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "workflow-vars",
		Short: "fail when a workflow reads a GitHub variable that no requirements list accounts for",
		Long: "Cross-checks every ${{ vars.X }} read by a shipped workflow against the two\n" +
			"lists in state.go: e2eRequirements (what `llz doctor` reports and `llz tokens`\n" +
			"provisions) and knownOptionalWorkflowVars (deliberately unset-able, with a\n" +
			"reason). A variable in neither is a setup trap: doctor never mentions it, and\n" +
			"the operator meets it as an empty value mid-CI-run.\n\n" +
			"Also fails in the other direction — an accounted variable no workflow reads any\n" +
			"more is toil an operator is still being asked to configure.\n\n" +
			"Runs offline — no GitHub API, no network.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runWorkflowVars(root, os.Stdout, os.Stderr)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root to scan")
	return c
}

// declaredWorkflowVars is the variable half of e2eRequirements. Secrets are
// excluded: this gate only reasons about `vars.*`.
func declaredWorkflowVars() map[string]bool {
	out := map[string]bool{}
	for _, r := range e2eRequirements(true) {
		if !r.Secret {
			out[r.Name] = true
		}
	}
	return out
}

// scanWorkflowVars maps each variable read by a shipped workflow to the files
// that read it.
func scanWorkflowVars(root string) (map[string][]string, error) {
	used := map[string]map[string]bool{}
	for _, r := range workflowVarRoots {
		start := filepath.Join(root, filepath.FromSlash(r))
		err := filepath.WalkDir(start, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".yml") && !strings.HasSuffix(d.Name(), ".yaml") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			for _, m := range reWorkflowVar.FindAllStringSubmatch(string(data), -1) {
				if used[m[1]] == nil {
					used[m[1]] = map[string]bool{}
				}
				used[m[1]][filepath.ToSlash(rel)] = true
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("workflow-vars: walk %s: %w", r, err)
		}
	}
	out := map[string][]string{}
	for name, files := range used {
		out[name] = sortedNames(files)
	}
	return out, nil
}

func runWorkflowVars(root string, out, errOut io.Writer) error {
	used, err := scanWorkflowVars(root)
	if err != nil {
		return err
	}
	if len(used) == 0 {
		return fmt.Errorf("workflow-vars: no ${{ vars.* }} found under %s — refusing to pass vacuously",
			strings.Join(workflowVarRoots, ", "))
	}
	declared := declaredWorkflowVars()

	var unaccounted, stale []string
	for _, name := range sortedNames(used) {
		if !declared[name] {
			if _, ok := knownOptionalWorkflowVars[name]; !ok {
				unaccounted = append(unaccounted, name)
			}
		}
	}
	for name := range knownOptionalWorkflowVars {
		if _, ok := used[name]; !ok {
			stale = append(stale, name)
		}
	}
	for name := range declared {
		if _, ok := used[name]; !ok {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)

	if len(unaccounted) == 0 && len(stale) == 0 {
		fmt.Fprintf(out, "workflow-vars: OK — %d variable(s) read by workflows, all accounted for (%d required, %d optional)\n",
			len(used), len(declared), len(knownOptionalWorkflowVars))
		return nil
	}

	for _, name := range unaccounted {
		fmt.Fprintf(errOut, "::error file=%s::${{ vars.%s }} is read here but appears in no requirements list\n",
			used[name][0], name)
	}
	if len(unaccounted) > 0 {
		fmt.Fprintf(errOut, "\n%s %d workflow variable(s) are read but unaccounted for:\n", red("✗"), len(unaccounted))
		for _, name := range unaccounted {
			fmt.Fprintf(errOut, "    %s\n        read by: %s\n", name, strings.Join(used[name], ", "))
		}
		fmt.Fprintf(errOut, "\n`llz doctor` reports against e2eRequirements, so a variable missing from it is\n"+
			"never surfaced — the operator meets it as an empty value mid-CI-run. Add each to\n"+
			"one of the two lists in state.go:\n"+
			"  • e2eRequirements          — an instance needs it; doctor reports it and tokens provisions it\n"+
			"  • knownOptionalWorkflowVars — unset is a working instance; record WHY in the value\n")
	}
	if len(stale) > 0 {
		fmt.Fprintf(errOut, "\n%s %d accounted variable(s) are no longer read by any workflow:\n", red("✗"), len(stale))
		for _, name := range stale {
			fmt.Fprintf(errOut, "    %s\n", name)
		}
		fmt.Fprintf(errOut, "\nDrop them from state.go — an operator is otherwise still being asked to\n"+
			"configure something nothing reads.\n")
	}
	return fmt.Errorf("workflow-vars: %d unaccounted, %d stale", len(unaccounted), len(stale))
}
