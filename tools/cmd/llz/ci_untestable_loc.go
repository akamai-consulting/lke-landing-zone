package main

// ci_untestable_loc.go implements `llz ci untestable-loc` — the design-principle
// gate this repo ratchets against: logic belongs in unit-testable Go, not in
// inline workflow bash, standalone shell scripts, or untested Python. The
// command counts "logic lines" (non-blank, non-comment) across three categories
// and fails when any category exceeds the budget declared in a committed config
// file (default .untestable-budget.yaml at the repo root). Budgets are meant to
// ratchet DOWN over time as more bash/python moves into the CLI; a PR that adds
// untestable code instead of converting it pushes a category over budget and is
// rejected in CI.
//
// The counting rules deliberately mirror the one-off measurement scripts used
// when the budgets were first set, so the numbers are reproducible:
//   workflow-inline-bash  every non-blank, non-comment line inside a `run:`
//                         block (`|`/`>` scalar or single-line) in a workflow
//                         or composite-action YAML
//   shell-scripts         non-blank, non-comment lines in *.sh
//   python-scripts        non-blank, non-comment lines in *.py
//   tf-provisioner-bash   non-blank, non-comment lines of bash inside
//                         `command = <<EOT … EOT` heredocs of Terraform
//                         local-exec / remote-exec provisioners
// The counters are pure functions (countRunBlockLines / countScriptLines /
// countTerraformProvisionerLines) with table-driven tests; the walk + budget
// comparison is the only I/O.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// untestableBudget is the on-disk config (.untestable-budget.yaml). Each
// category names the globs it scans and the maximum logic lines it tolerates;
// exclude removes files (install/glue scripts with no real logic) from every
// category so the budget reflects only convertible logic.
type untestableBudget struct {
	Categories map[string]untestableCategory `json:"categories"`
	Exclude    []string                      `json:"exclude,omitempty"`
}

type untestableCategory struct {
	// Kind selects the counter: "workflow-run" parses run: blocks out of YAML;
	// "script" counts non-blank/non-comment lines of the whole file;
	// "terraform-provisioner" parses `command` heredocs out of *.tf provisioners.
	Kind    string   `json:"kind"`
	Budget  int      `json:"budget"`
	Include []string `json:"include"`
}

// categoryResult is the per-category tally the gate reports.
type categoryResult struct {
	name   string
	kind   string
	budget int
	total  int
	files  []fileCount // sorted desc by count, for the offender breakdown
}

type fileCount struct {
	path  string
	count int
}

func (r categoryResult) over() bool { return r.total > r.budget }

func ciUntestableLOCCmd() *cobra.Command {
	var configPath, root string
	var verbose bool
	c := &cobra.Command{
		Use:   "untestable-loc",
		Short: "fail when inline-bash / shell / python / tf-provisioner logic exceeds the committed budget",
		Long: "Counts logic lines (non-blank, non-comment) of inline workflow bash,\n" +
			"shell scripts, Python scripts, and Terraform provisioner heredocs, and\n" +
			"fails if any category exceeds the budget in .untestable-budget.yaml. The\n" +
			"design principle: logic belongs in unit-testable Go (tools/), not in CI\n" +
			"shell. Budgets ratchet DOWN as code is converted — a PR that grows a\n" +
			"category over budget is rejected so reviewers can ask for the logic to\n" +
			"move into the llz CLI (or, for genuine install/glue, an explicit entry\n" +
			"under `exclude:`).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runUntestableLOC(root, configPath, verbose)
		},
	}
	c.Flags().StringVar(&configPath, "config", ".untestable-budget.yaml", "budget config file (relative to --root)")
	c.Flags().StringVar(&root, "root", ".", "repository root to scan")
	c.Flags().BoolVar(&verbose, "verbose", false, "list every file's count, not just over-budget categories")
	return c
}

func runUntestableLOC(root, configPath string, verbose bool) error {
	cfg, err := loadUntestableBudget(filepath.Join(root, configPath))
	if err != nil {
		return err
	}
	results, err := scanUntestable(root, cfg)
	if err != nil {
		return err
	}

	var overBudget []string
	for _, r := range results {
		status := "ok"
		if r.over() {
			status = "OVER"
			overBudget = append(overBudget, r.name)
		}
		fmt.Printf("%-26s %5d / %-5d  %s\n", r.name, r.total, r.budget, status)
		if verbose || r.over() {
			for _, f := range r.files {
				fmt.Printf("    %5d  %s\n", f.count, f.path)
			}
		}
	}

	if len(overBudget) > 0 {
		fmt.Fprintf(os.Stderr,
			"\n::error::untestable-loc: %s over budget. Move the logic into unit-tested Go "+
				"(tools/cmd/llz), or — for genuine install/glue with no logic — add the file to "+
				"`exclude:` in %s with a justification. Do NOT raise the budget to make this pass.\n",
			strings.Join(overBudget, ", "), configPath)
		return fmt.Errorf("untestable-loc gate failed: %d category(ies) over budget", len(overBudget))
	}
	fmt.Println("\nuntestable-loc: all categories within budget.")
	return nil
}

func loadUntestableBudget(path string) (untestableBudget, error) {
	var cfg untestableBudget
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read budget config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse budget config %s: %w", path, err)
	}
	if len(cfg.Categories) == 0 {
		return cfg, fmt.Errorf("budget config %s defines no categories", path)
	}
	return cfg, nil
}

// scanUntestable walks the repo once and tallies each category. Returns results
// sorted by category name for stable output.
func scanUntestable(root string, cfg untestableBudget) ([]categoryResult, error) {
	// Collect candidate files per category by walking once and matching globs.
	perCat := map[string][]string{}
	for name := range cfg.Categories {
		perCat[name] = nil
	}

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendored / VCS / build dirs outright.
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" || base == ".claude" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if matchAnyGlob(cfg.Exclude, rel) {
			return nil
		}
		for name, cat := range cfg.Categories {
			if matchAnyGlob(cat.Include, rel) {
				perCat[name] = append(perCat[name], rel)
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	var results []categoryResult
	for name, cat := range cfg.Categories {
		r := categoryResult{name: name, kind: cat.Kind, budget: cat.Budget}
		for _, rel := range perCat[name] {
			b, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				return nil, err
			}
			var n int
			switch cat.Kind {
			case "workflow-run":
				n = countRunBlockLines(string(b))
			case "script":
				n = countScriptLines(string(b))
			case "terraform-provisioner":
				n = countTerraformProvisionerLines(string(b))
			default:
				return nil, fmt.Errorf("category %q has unknown kind %q (want workflow-run|script|terraform-provisioner)", name, cat.Kind)
			}
			if n > 0 {
				r.files = append(r.files, fileCount{path: rel, count: n})
				r.total += n
			}
		}
		sort.Slice(r.files, func(i, j int) bool {
			if r.files[i].count != r.files[j].count {
				return r.files[i].count > r.files[j].count
			}
			return r.files[i].path < r.files[j].path
		})
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].name < results[j].name })
	return results, nil
}

var runDirectiveRE = regexp.MustCompile(`^(\s*)(- )?run:\s*(.*)$`)

// countRunBlockLines counts non-blank, non-comment lines inside every `run:`
// block of a workflow / composite-action YAML document. Handles both the
// block-scalar form (`run: |` / `run: >` followed by an indented body) and the
// single-line form (`run: some-command`).
func countRunBlockLines(content string) int {
	lines := strings.Split(content, "\n")
	total := 0
	for i := 0; i < len(lines); i++ {
		m := runDirectiveRE.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		indent := len(m[1])
		rest := strings.TrimSpace(m[3])
		isBlock := rest == "" || rest[0] == '|' || rest[0] == '>'
		if !isBlock {
			// Single-line command (`run: llz ci foo`) is tool-invocation glue,
			// not embedded logic — it's exactly what a converted step looks
			// like, so counting it would penalize the conversions this gate
			// exists to encourage. Only multi-line `run:` blocks (which hold
			// real logic) are counted.
			continue
		}
		// Block scalar: count LOGICAL lines of the body until the indentation
		// returns to <= the run: directive's own indent. Backslash-continued
		// commands count once — a `llz ci foo --a \ --b \ --c` invocation that
		// wraps across physical lines is one tool call (glue), the same shape a
		// converted step takes, so counting each wrapped line would penalize the
		// conversions this gate rewards.
		prevContinues := false
		for i++; i < len(lines); i++ {
			l := lines[i]
			if strings.TrimSpace(l) == "" {
				continue
			}
			if lineIndent(l) <= indent {
				i-- // re-examine this line in the outer loop (could be another run:)
				break
			}
			s := strings.TrimSpace(l)
			isComment := strings.HasPrefix(s, "#")
			if !isComment && !prevContinues {
				total++
			}
			// A comment line never continues a command; otherwise a trailing
			// backslash marks the next physical line as a continuation.
			prevContinues = !isComment && strings.HasSuffix(s, `\`)
		}
	}
	return total
}

// countScriptLines counts non-blank lines that are not whole-line comments.
// Mirrors `grep -vE '^\s*(#|$)'` so shell and Python tallies are reproducible.
func countScriptLines(content string) int {
	total := 0
	for _, l := range strings.Split(content, "\n") {
		s := strings.TrimSpace(l)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		total++
	}
	return total
}

// tfCommandHeredocRE matches the opening of a Terraform provisioner command
// heredoc: `command = <<EOT` or the indent-stripping `command = <<-EOT`. The
// captured group is the terminator tag.
var tfCommandHeredocRE = regexp.MustCompile(`^\s*command\s*=\s*<<-?(\w+)\s*$`)

// countTerraformProvisionerLines counts non-blank, non-comment logic lines of
// bash embedded in `command = <<EOT … EOT` heredocs of Terraform local-exec /
// remote-exec provisioners. A single-line `command = "./script.sh"` invocation
// is glue — it shells out to a script already counted under shell-scripts and
// looks exactly like a converted step — so only heredoc bodies (the inline
// logic this gate exists to push into Go) are counted. As in countRunBlockLines,
// backslash-continued commands count once so wrapping a long tool call across
// physical lines is not penalized. Anchoring on the `command` attribute (rather
// than every heredoc) keeps description/output/policy heredocs out of the tally.
func countTerraformProvisionerLines(content string) int {
	lines := strings.Split(content, "\n")
	total := 0
	for i := 0; i < len(lines); i++ {
		m := tfCommandHeredocRE.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		tag := m[1]
		prevContinues := false
		for i++; i < len(lines); i++ {
			s := strings.TrimSpace(lines[i])
			if s == tag {
				break // closing delimiter — leave the outer loop to scan onward
			}
			if s == "" {
				continue
			}
			isComment := strings.HasPrefix(s, "#")
			if !isComment && !prevContinues {
				total++
			}
			prevContinues = !isComment && strings.HasSuffix(s, `\`)
		}
	}
	return total
}

func lineIndent(l string) int {
	return len(l) - len(strings.TrimLeft(l, " "))
}

func matchAnyGlob(patterns []string, path string) bool {
	for _, p := range patterns {
		if matchGlob(p, path) {
			return true
		}
	}
	return false
}

// matchGlob matches a slash-path against a glob supporting `**` (any number of
// path segments, including zero), `*` (within a segment), and `?`. Anchored at
// both ends. filepath.Match lacks `**`, so we compile to a regexp.
func matchGlob(pattern, path string) bool {
	re, err := globToRegexp(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(path)
}

var globCache = map[string]*regexp.Regexp{}

func globToRegexp(pattern string) (*regexp.Regexp, error) {
	if re, ok := globCache[pattern]; ok {
		return re, nil
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// `**` — any sequence including slashes. Swallow a following
				// slash so `a/**/b` also matches `a/b`.
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*") // single * stays within a path segment
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	globCache[pattern] = re
	return re, nil
}
