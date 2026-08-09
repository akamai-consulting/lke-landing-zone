package budget

// Package budget is the engine shared by every budget gate.
//
// A budget gate is the same shape every time: walk the repo once, tally matching
// files per category with a counter chosen by `kind:`, compare each total to a
// committed ceiling, and fail with guidance when one is over. Two gates use it,
// and they push in OPPOSITE directions:
//
//	llz ci untestable-loc  (.untestable-budget.yaml)  caps logic that cannot be
//	                       unit-tested; its remedy is "move it INTO unit-tested Go
//	                       under tools/internal".
//	llz ci core-surface    (.core-surface-budget.yaml) caps the CLI wiring layer
//	                       (tools/internal/cli) and, separately, the six-line
//	                       tools/cmd/llz entry point; its remedy is "move it OUT"
//	                       of the layer that only wires commands up (ADR 0014).
//
// BOTH NOW NAME tools/internal, WHICH IS NOT A COLLISION — see remedy.go's header
// for why the axes are still opposite (language vs. layer) and for the bug that
// wording it as one axis produced.
//
// That opposition is why the remedy is configuration rather than a constant, and
// why the two keep separate config files and separate commands: a single gate
// whose breach message said both things would be a riddle, not a gate.
//
// Everything here is deliberately gate-neutral. A third budget should need a
// config file, a counter and a command — not a fork of this file.
//
// IT LIVES OUTSIDE THE WIRING LAYER BECAUSE ITS OWN GATE SAID SO. `llz ci
// core-surface` counts the Go logic in tools/internal/cli and prescribes exactly
// this move; running the gate on itself and then taking its advice is the cheapest
// available proof that the advice is followable. The two cobra commands stay in
// the wiring layer — a flag set and a remedy string each — and everything with a
// test moved here. See docs/designs/internal-extensions.md, which lists
// `guard-budgets` as the first extraction for this reason.

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/pathglob"
)

// budgetConfig is the on-disk config for a budget gate. Each category names
// the globs it scans and the maximum logic lines it tolerates; exclude removes
// files (install/glue scripts with no real logic) from every category so the
// budget reflects only convertible logic.
//
// TWO GATES SHARE THIS ENGINE, AND THEY PUSH ALONG DIFFERENT AXES.
// .untestable-budget.yaml caps logic that CANNOT be unit-tested, and its remedy
// is "move it into unit-tested Go under tools/internal" — a LANGUAGE axis.
// .core-surface-budget.yaml caps the CLI wiring layer, and its remedy is "move it
// OUT of the layer that only wires commands up" (ADR 0014) — a LAYER axis. The
// scan, the tally and the ratchet doctrine are identical; only the remedy sentence
// differs, which is why Remedy is config and not a constant.
type budgetConfig struct {
	Categories map[string]budgetCategory `json:"categories"`
	Exclude    []string                  `json:"exclude,omitempty"`
	// Remedy is the guidance printed when a category is over budget. Empty means
	// the untestable-loc wording, so .untestable-budget.yaml needs no new key.
	Remedy string `json:"remedy,omitempty"`
}

type budgetCategory struct {
	// Kind selects the counter: "workflow-run" parses run: blocks out of YAML;
	// "script" counts non-blank/non-comment lines of the whole file;
	// "terraform-provisioner" parses `command` heredocs out of *.tf provisioners;
	// "go-logic" counts non-blank/non-comment lines of Go.
	Kind    string   `json:"kind"`
	Budget  int      `json:"budget"`
	Include []string `json:"include"`
	// AllowEmpty permits a category to match no files. It must be declared, because
	// silence is otherwise indistinguishable from success — see matchedNothing.
	AllowEmpty bool `json:"allowEmpty,omitempty"`
	// Exact requires Budget to EQUAL the measurement, not merely bound it — see
	// categoryResult.stale. Opt-in, because .untestable-budget.yaml deliberately
	// carries ~3% headroom and must keep it.
	Exact bool `json:"exact,omitempty"`
}

// categoryResult is the per-category tally the gate reports.
type categoryResult struct {
	name    string
	kind    string
	budget  int
	total   int
	files   []fileCount // sorted desc by count, for the offender breakdown
	matched int         // scaffold files the include globs actually matched
	exact   bool        // budget must equal the measurement, not merely bound it
	// allowEmpty mirrors the category's opt-in, so the verdict needs only the result.
	allowEmpty bool
}

type fileCount struct {
	path  string
	count int
}

func (r categoryResult) over() bool { return r.total > r.budget }

// matchedNothing reports a category whose globs matched no file at all — a gate
// that cannot fail, reported as a triumphant `0 / <budget> ok`.
//
// This is the failure the sibling coverage guard already refuses to allow: `llz
// ci check-coverage` fails when a gated package "produced no coverage data at
// all (a renamed/removed package must not pass silently)". The same hazard is
// sharper here, because ZERO IS THE NUMBER THIS GATE EXISTS TO CELEBRATE. ADR
// 0014's whole thesis is driving package main from ~41,700 toward ~3,000, and its
// prescribed remedy is moving code OUT of tools/cmd/llz — so a stale glob and a
// spectacular success produce byte-identical output, and the better the
// decomposition looks the less anyone questions a small number. A one-character
// typo (`llz` -> `IIz`) was enough to retire the gate permanently while it kept
// printing `ok`.
//
// A category that is legitimately empty says so with `allowEmpty: true`, which
// makes the claim reviewable instead of implicit.
func (r categoryResult) matchedNothing() bool { return r.matched == 0 && !r.allowEmpty }

// stale reports an `exact` category whose budget sits ABOVE the measurement —
// slack that nobody declared.
//
// Enforcing only `total <= budget` was not enough to make the high-water model
// hold. That model's claim is "the number equals the measurement", and the value
// of the claim is that there is nowhere for growth to hide. But a PR that SHRINKS
// package main and forgets to lower the line leaves slack behind silently, the
// next change grows into it unremarked, and the whole thing degrades back into
// the ceiling-with-headroom design the high-water mark replaced — degrading
// exactly when decomposition succeeds, which is the outcome ADR 0014 exists to
// cause. Extract guard-budgets, forget this line, and the next 646 lines of
// growth are pre-approved.
//
// So a reduction is a failure too, with the new number in the message. That makes
// the downward ratchet automatic instead of aspirational: paying the budget down
// is the one thing the gate will not let you do by accident.
func (r categoryResult) stale() bool { return r.exact && r.matched > 0 && r.total < r.budget }

// Run loads a budget config, tallies every category and fails when any is over.
// Shared by `llz ci untestable-loc` and `llz ci core-surface`: the two gates
// differ only in which config they read and what they tell you to do about a
// breach, so everything else lives here once.
//
// Gate names the gate in output and errors; DefaultRemedy is the guidance printed
// on a breach when the config declares no `remedy:` key of its own.
func Run(gate, root, configPath string, verbose bool, defaultRemedy string) error {
	return RunTo(gate, root, configPath, verbose, defaultRemedy, os.Stdout, os.Stderr)
}

// RunTo is Run with its output injected.
//
// The writers are parameters because the gate's OUTPUT IS ITS PRODUCT. Everything
// this command exists to do — name the over-budget category, print the remedy
// that makes the ratchet teach rather than merely block, and emit a `::error::`
// annotation GitHub will render on the diff — happens on these two streams, and
// none of it was assertable while they were package-level. Two mutants proved it:
// deleting the `{config}` substitution (operators get a literal "{config}") and
// replacing the `::error::` prefix (the annotation silently stops rendering while
// the build still exits 1) both left the suite green. That second one is the exact
// hazard ci_guards.go documents — GitHub only parses an annotation command at the
// START of a line — which is why that file keeps its logic in pure functions over
// injected io.Writers. This one now does too.
func RunTo(gate, root, configPath string, verbose bool, defaultRemedy string, out, errOut io.Writer) error {
	repo := capability.RepoForGate(Extension(), root)
	cfg, err := loadBudgetConfig(repo, configPath)
	if err != nil {
		return err
	}
	results, err := scanBudgetCategories(repo, cfg)
	if err != nil {
		return err
	}

	var overBudget, dead []string
	var stale []categoryResult
	for _, r := range results {
		status := "ok"
		switch {
		case r.matchedNothing():
			status = "MATCHED NOTHING"
			dead = append(dead, r.name)
		case r.over():
			status = "OVER"
			overBudget = append(overBudget, r.name)
		case r.stale():
			status = "SHRANK — LOWER IT"
			stale = append(stale, r)
		}
		fmt.Fprintf(out, "%-26s %5d / %-5d  %s\n", r.name, r.total, r.budget, status)
		if verbose || r.over() {
			for _, f := range r.files {
				fmt.Fprintf(out, "    %5d  %s\n", f.count, f.path)
			}
		}
	}

	// A dead category is reported BEFORE an over-budget one and fails on its own.
	// A gate that cannot fail is worse than no gate: it reports `0 / <budget> ok`
	// forever and everyone downstream reads that as compliance.
	if len(dead) > 0 {
		fmt.Fprintf(errOut,
			"\n::error::%s: %s matched NO files. The include globs in %s are stale, so this "+
				"category can never fail — it will report a passing 0 indefinitely. Fix the globs, "+
				"or declare `allowEmpty: true` on the category if it is legitimately empty.\n",
			gate, strings.Join(dead, ", "), configPath)
		return fmt.Errorf("%s gate failed: %d category(ies) matched no files", gate, len(dead))
	}

	if len(overBudget) > 0 {
		remedy := cfg.Remedy
		if remedy == "" {
			remedy = defaultRemedy
		}
		fmt.Fprintf(errOut,
			"\n::error::%s: %s over budget. %s\n",
			gate, strings.Join(overBudget, ", "), strings.ReplaceAll(remedy, "{config}", configPath))
		return fmt.Errorf("%s gate failed: %d category(ies) over budget", gate, len(overBudget))
	}

	// Reported LAST: a shrink is good news, and it should not shout over a breach
	// that is actually blocking someone. It still fails, because the number is only
	// a high-water mark for as long as it tracks reality.
	if len(stale) > 0 {
		fmt.Fprintf(errOut, "\n::error::%s: package shrank and the committed number did not.\n", gate)
		for _, r := range stale {
			fmt.Fprintf(errOut, "::error::  %s: measured %d, %s says %d — lower it to %d.\n",
				r.name, r.total, configPath, r.budget, r.total)
		}
		fmt.Fprintf(errOut,
			"::error::This is a paydown, so bank it: an `exact` budget must EQUAL the measurement, "+
				"or the slack it leaves behind silently pre-approves the next change's growth.\n")
		return fmt.Errorf("%s gate failed: %d category(ies) below an exact budget", gate, len(stale))
	}
	fmt.Fprintf(out, "\n%s: all categories within budget.\n", gate)
	return nil
}

func loadBudgetConfig(repo capability.Repo, path string) (budgetConfig, error) {
	var cfg budgetConfig
	b, err := repo.ReadFile(path)
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

// scanBudgetCategories walks the repo once and tallies each category. Returns results
// sorted by category name for stable output.
func scanBudgetCategories(repo capability.Repo, cfg budgetConfig) ([]categoryResult, error) {
	// Collect candidate files per category by walking once and matching globs.
	perCat := map[string][]string{}
	for name := range cfg.Categories {
		perCat[name] = nil
	}

	walkErr := repo.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
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
		// Already repo-relative — the reader is fenced to the root and expresses
		// everything under it, so the filepath.Rel that derived this is gone.
		rel := filepath.ToSlash(path)
		if pathglob.MatchAny(cfg.Exclude, rel) {
			return nil
		}
		for name, cat := range cfg.Categories {
			if pathglob.MatchAny(cat.Include, rel) {
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
		// matched counts files the globs SELECTED, not files that scored > 0. A
		// category of genuinely comment-only files tallies 0 and is still alive;
		// one whose globs match nothing is dead. Conflating them is what let a
		// typo'd glob pass as `0 / budget ok`.
		r := categoryResult{name: name, kind: cat.Kind, budget: cat.Budget,
			matched: len(perCat[name]), allowEmpty: cat.AllowEmpty, exact: cat.Exact}
		for _, rel := range perCat[name] {
			b, err := repo.ReadFile(rel)
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
			case "makefile-recipe":
				n = countMakefileRecipeLines(string(b))
			case "embedded-shell":
				n = countEmbeddedShellLines(string(b))
			case "go-logic":
				n = countGoLogicLines(string(b))
			default:
				return nil, fmt.Errorf("category %q has unknown kind %q (want workflow-run|script|terraform-provisioner|makefile-recipe|embedded-shell|go-logic)", name, cat.Kind)
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

// countLinesSkippingComments is the tally shared by the whole-file counters:
// non-blank lines that do not begin with the language's comment marker.
func countLinesSkippingComments(content, comment string) int {
	total := 0
	for _, l := range strings.Split(content, "\n") {
		s := strings.TrimSpace(l)
		if s == "" || strings.HasPrefix(s, comment) {
			continue
		}
		total++
	}
	return total
}
