package main

// ci_chart_guard.go implements `llz ci chart-version-guard` — the release-hygiene
// check that fails a PR when a file under a chart directory changes without that
// chart's Chart.yaml `version:` being bumped.
//
// Why it exists: publish-charts.yml publishes immutably — it pushes a chart only
// when its version is not already in the registry and never overwrites a tag. So
// a template/values change merged WITHOUT a version bump is silently never
// published; clusters keep pulling the stale artifact by their pinned
// targetRevision. That is how the firewall-controller runner-acl RBAC grant
// reached chart source but never any cluster, leaving the control-plane ACL
// un-reconciled. This guard turns that class of mistake into a red PR.
//
// The decision logic (which dirs changed, and whether each bumped its version)
// is pure and unit-tested; git is reached only through the execOutput seam.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

const chartsRoot = "kubernetes-charts/"

func ciChartVersionGuardCmd() *cobra.Command {
	var base, root string
	c := &cobra.Command{
		Use:   "chart-version-guard",
		Short: "fail when a chart changes without bumping its Chart.yaml version",
		Long: "Diffs each kubernetes-charts/<chart>/ directory this PR touches against the\n" +
			"PR base and fails if that chart's Chart.yaml version: is unchanged. publish-\n" +
			"charts.yml publishes immutably (only a new version is pushed), so a chart change\n" +
			"merged without a version bump is never published and clusters keep the stale\n" +
			"chart. New charts (no Chart.yaml at the base) and removed charts are exempt.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runChartVersionGuard(base, root)
		},
	}
	c.Flags().StringVar(&base, "base", "", "git ref/SHA of the PR base to diff against (required)")
	c.Flags().StringVar(&root, "root", ".", "repository root")
	return c
}

func runChartVersionGuard(base, root string) error {
	if base == "" {
		return fmt.Errorf("--base (PR base ref/SHA) is required")
	}

	diff, err := gitOutput(root, "diff", "--name-only", base+"...HEAD", "--", chartsRoot)
	if err != nil {
		return fmt.Errorf("git diff against base %s: %w", base, err)
	}
	dirs := changedChartDirs(splitLines(diff))
	if len(dirs) == 0 {
		fmt.Println("No chart directories changed.")
		return nil
	}

	var failed []string
	for _, dir := range dirs {
		// New version from the working tree; "" when the chart was removed.
		newVer := chartVersion(readFileOrEmpty(filepath.Join(root, dir, "Chart.yaml")))
		// Old version from the base; "" when the chart is new (or had none).
		oldRaw, _ := gitOutput(root, "show", base+":"+dir+"/Chart.yaml")
		oldVer := chartVersion(oldRaw)

		ok, msg := classifyChartBump(dir, oldVer, newVer)
		if ok {
			fmt.Println("  " + msg)
			continue
		}
		fmt.Fprintf(os.Stderr, "::error file=%s/Chart.yaml::%s\n", dir, msg)
		failed = append(failed, dir)
	}

	if len(failed) > 0 {
		return fmt.Errorf("chart-version-guard: %d chart(s) changed without a version bump: %s",
			len(failed), strings.Join(failed, ", "))
	}
	fmt.Println("chart-version-guard: every changed chart bumped its version.")
	return nil
}

// changedChartDirs reduces a list of changed file paths to the unique, sorted set
// of kubernetes-charts/<chart> directories they live in. Files directly under
// kubernetes-charts/ (e.g. a README) belong to no chart and are ignored.
func changedChartDirs(files []string) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		if !strings.HasPrefix(f, chartsRoot) {
			continue
		}
		rest := f[len(chartsRoot):]
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 {
			continue // no chart subdirectory component
		}
		dir := chartsRoot + rest[:slash]
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs
}

// classifyChartBump decides whether a touched chart satisfies the bump rule.
// ok=false (with an actionable message) only when the chart still exists, existed
// at the base, and its version is unchanged. A removed chart (newVer == "") and a
// brand-new chart (oldVer == "") are both exempt — there is no prior published
// artifact to collide with.
func classifyChartBump(dir, oldVer, newVer string) (ok bool, msg string) {
	switch {
	case newVer == "":
		return true, dir + ": removed — skipping"
	case oldVer == "":
		return true, fmt.Sprintf("%s: new chart (version %s) — no prior version to compare", dir, newVer)
	case newVer == oldVer:
		return false, fmt.Sprintf("%s changed but Chart.yaml version is still %s — bump version: to publish "+
			"(publish-charts.yml only pushes a NEW version, so an unbumped change is never released)", dir, newVer)
	default:
		return true, fmt.Sprintf("%s: %s → %s", dir, oldVer, newVer)
	}
}

// chartVersion extracts the top-level `version:` value from Chart.yaml content,
// or "" when absent. Only column-0 `version:` matches, so nested keys and
// `appVersion:` are not mistaken for it.
func chartVersion(chartYAML string) string {
	for _, line := range strings.Split(chartYAML, "\n") {
		if strings.HasPrefix(line, "version:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "version:"))
		}
	}
	return ""
}

// splitLines splits git output into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// readFileOrEmpty returns the file's contents, or "" if it cannot be read (e.g.
// the chart was removed in this PR).
func readFileOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
