package main

// ci_chart_lock_guard.go implements `llz ci chart-lock-drift` — the native port
// of template-scripts/linting-and-validation/check-chart-lock-drift.py (the
// Makefile's helm-dep-lock-check). It verifies that every named chart's
// committed Chart.lock matches the dependency declarations in its Chart.yaml:
// a dependency name, version, or repository that differs means Chart.yaml was
// edited without re-running `helm dependency update`, so the lock is stale.
//
// The comparison core (checkChartLock) is pure and unit-tested; RunE only reads
// the two files per chart and prints.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// chartDep is one entry of a Chart.yaml `dependencies:` / Chart.lock list.
type chartDep struct {
	Name       string `yaml:"name"`
	Version    string `yaml:"version"`
	Repository string `yaml:"repository"`
}

type chartDepDoc struct {
	Dependencies []chartDep `yaml:"dependencies"`
}

// chartLockResult is the outcome of checking one chart directory.
type chartLockResult struct {
	Dir      string
	Skipped  bool // no declared dependencies — nothing to lock
	Errors   []string
	Warnings []string
}

func ciChartLockDriftCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "chart-lock-drift <chart-dir>...",
		Short: "fail when a chart's committed Chart.lock drifts from its Chart.yaml dependencies",
		Long: "Native port of check-chart-lock-drift.py (the Makefile's helm-dep-lock-check).\n" +
			"For each chart directory, compares Chart.lock against the dependency\n" +
			"declarations in Chart.yaml and fails if any dependency's name, version, or\n" +
			"repository differs (or Chart.lock is missing) — meaning Chart.yaml was updated\n" +
			"without re-running `helm dependency update`.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runChartLockDrift(root, args, os.Stdout)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root the chart dirs are relative to")
	return c
}

func runChartLockDrift(root string, dirs []string, out io.Writer) error {
	total := 0
	for _, dir := range dirs {
		full := filepath.Join(root, dir)
		chartYAML, chartErr := os.ReadFile(filepath.Join(full, "Chart.yaml"))
		lockRaw, lockErr := os.ReadFile(filepath.Join(full, "Chart.lock"))

		var chartPtr, lockPtr *string
		if chartErr == nil {
			s := string(chartYAML)
			chartPtr = &s
		}
		if lockErr == nil {
			s := string(lockRaw)
			lockPtr = &s
		}

		res := checkChartLock(dir, chartPtr, lockPtr)
		for _, w := range res.Warnings {
			fmt.Fprintf(os.Stderr, "::warning file=%s/Chart.lock::%s\n", dir, w)
		}
		for _, e := range res.Errors {
			fmt.Fprintf(os.Stderr, "::error file=%s/Chart.yaml::%s\n", dir, e)
			total++
		}
		switch {
		case res.Skipped:
			fmt.Fprintf(out, "  skip (no dependencies): %s\n", dir)
		case len(res.Errors) == 0:
			fmt.Fprintf(out, "  ok: %s\n", dir)
		}
	}

	if total > 0 {
		return fmt.Errorf("chart-lock-drift: %d Chart.lock drift error(s) found "+
			"(run `helm dependency update <chart>` and commit the result)", total)
	}
	fmt.Fprintln(out, "All Chart.lock files are in sync with Chart.yaml.")
	return nil
}

// checkChartLock compares a chart's Chart.yaml against its Chart.lock. A nil
// raw pointer means that file is absent. The returned errors/warnings are bare
// messages (the caller adds the ::error::/::warning:: annotation wrapper).
func checkChartLock(dir string, chartYAML, lockRaw *string) chartLockResult {
	res := chartLockResult{Dir: dir}

	if chartYAML == nil {
		res.Errors = append(res.Errors, fmt.Sprintf("No Chart.yaml found in %s", dir))
		return res
	}

	var chartDoc chartDepDoc
	if err := yaml.Unmarshal([]byte(*chartYAML), &chartDoc); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("%s: failed to parse Chart.yaml: %v", dir, err))
		return res
	}
	declared := map[string]chartDep{}
	for _, d := range chartDoc.Dependencies {
		declared[d.Name] = d
	}

	if len(declared) == 0 {
		res.Skipped = true
		return res
	}

	if lockRaw == nil {
		res.Errors = append(res.Errors, fmt.Sprintf(
			"%s: Chart.lock is missing. Run `helm dependency update %s` and commit the result.", dir, dir))
		return res
	}

	var lockDoc chartDepDoc
	if err := yaml.Unmarshal([]byte(*lockRaw), &lockDoc); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("%s: failed to parse Chart.lock: %v", dir, err))
		return res
	}
	locked := map[string]chartDep{}
	for _, d := range lockDoc.Dependencies {
		locked[d.Name] = d
	}

	for _, name := range sortedDepKeys(declared) {
		d := declared[name]
		l, ok := locked[name]
		if !ok {
			res.Errors = append(res.Errors, fmt.Sprintf(
				"%s: dependency '%s' is declared in Chart.yaml but missing from Chart.lock. "+
					"Run `helm dependency update %s`.", dir, name, dir))
			continue
		}
		if d.Version != l.Version {
			res.Errors = append(res.Errors, fmt.Sprintf(
				"%s: dependency '%s' version mismatch — Chart.yaml: %q, Chart.lock: %q. "+
					"Run `helm dependency update %s`.", dir, name, d.Version, l.Version, dir))
		}
		if d.Repository != l.Repository {
			res.Errors = append(res.Errors, fmt.Sprintf(
				"%s: dependency '%s' repository mismatch — Chart.yaml: %q, Chart.lock: %q. "+
					"Run `helm dependency update %s`.", dir, name, d.Repository, l.Repository, dir))
		}
	}

	for _, name := range sortedDepKeys(locked) {
		if _, ok := declared[name]; !ok {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%s: Chart.lock contains '%s' which is not in Chart.yaml — stale lock entry.", dir, name))
		}
	}

	return res
}

func sortedDepKeys(m map[string]chartDep) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
