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

	// Which charts changed, unioned from three sources. The committed diff alone
	// is all CI needs (its tree is clean), but newVer below is read from the
	// WORKING TREE — so with only the committed diff a local pre-commit run
	// compares the old chart set against new on-disk files and prints "No chart
	// directories changed" for a chart the very next commit fails on. That false
	// green is the whole reason the other two sources are here.
	committed, err := gitOutput(root, "diff", "--name-only", base+"...HEAD", "--", chartsRoot)
	if err != nil {
		return fmt.Errorf("git diff against base %s: %w", base, err)
	}
	// Staged + unstaged edits to tracked files, then untracked additions (a new
	// chart, or a new template inside an existing one). Errors are discarded on
	// purpose and are safe to discard here, unlike the base ref below: both
	// commands are working-tree queries that need no history, so the only way
	// they fail is a broken repo the committed diff above would already have
	// errored on. A failure degrades to CI's committed-only view, never to a
	// pass-having-compared-nothing.
	worktree, _ := gitOutput(root, "diff", "--name-only", "HEAD", "--", chartsRoot)
	untracked, _ := gitOutput(root, "ls-files", "--others", "--exclude-standard", "--", chartsRoot)

	dirs := changedChartDirs(splitLines(committed + "\n" + worktree + "\n" + untracked))
	if len(dirs) == 0 {
		fmt.Println("No chart directories changed (working tree included).")
		return nil
	}

	// Resolve the base ONCE before comparing anything. Per-chart `git show` errors
	// are discarded below because "path absent at base" is how a genuinely new
	// chart looks — but that same empty result is what a bad base ref, a shallow
	// clone missing the base commit, or any git failure produces. Without this
	// check every chart would look brand-new, and classifyChartBump exempts new
	// charts from the bump requirement: the guard would pass the entire changeset
	// having compared nothing.
	if _, err := gitOutput(root, "rev-parse", "--verify", base+"^{commit}"); err != nil {
		return fmt.Errorf("chart-version-guard: base ref %q does not resolve (%v) — every chart would look new and skip the bump check; "+
			"fetch the base commit (actions/checkout needs fetch-depth: 0 or an explicit base fetch)", base, err)
	}

	var failed []string
	for _, dir := range dirs {
		// New version from the working tree; "" when the chart was removed.
		newVer := chartVersion(readFileOrEmpty(filepath.Join(root, dir, "Chart.yaml")))
		// Old version from the base; "" when the chart is new. Safe to discard the
		// error now that the base ref itself is known good.
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
	fmt.Println("  next: `make chart-pin-guard` — a bump leaves the Argo pins on the OLD version, " +
		"and a pin the registry never received 404s at Argo sync time.")
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
			"(publish-charts.yml only pushes a NEW version, so an unbumped change is never released), "+
			"then realign the Argo pins with `make chart-pin-guard` — the bump is only half done until they match", dir, newVer)
	default:
		return true, fmt.Sprintf("%s: %s → %s", dir, oldVer, newVer)
	}
}

// chartVersion extracts the top-level `version:` value from Chart.yaml content,
// or "" when absent. Only column-0 `version:` matches, so nested keys and
// `appVersion:` are not mistaken for it.
func chartVersion(chartYAML string) string { return chartScalar(chartYAML, "version:") }

// chartScalar reads a column-0 `<key> <value>` scalar out of Chart.yaml, with
// surrounding quotes stripped.
//
// The quote stripping is the point: `version: "0.1.11"` is valid YAML, and the
// PIN side of every comparison already strips quotes (extractChartPins,
// siblingValue). Without it here, quoting a chart version makes chart-pin-guard
// compare `"0.1.11"` against `0.1.11` and report a drift that does not exist —
// a spurious failure on a legal edit. Latent today (no chart is quoted), and
// cheaper to make symmetric than to discover.
//
// Column-0 only, so nested keys and `appVersion:` are never mistaken for it.
func chartScalar(chartYAML, key string) string {
	for _, line := range strings.Split(chartYAML, "\n") {
		if strings.HasPrefix(line, key) {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, key)), `"'`)
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
