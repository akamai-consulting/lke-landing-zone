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
//   embedded-shell        non-blank, non-comment lines of shell embedded in a
//                         YAML block scalar — a ConfigMap `data` entry like
//                         `relabel.sh: |` or an Argo `script.source: |`. These
//                         hide from the *.sh glob (the file is YAML), so they
//                         get their own counter that extracts the block body.
// The counters are pure functions (countRunBlockLines / countScriptLines /
// countTerraformProvisionerLines / countEmbeddedShellLines) with table-driven
// tests, and they are all this file owns besides the command itself.
//
// The machinery underneath — config parse, glob walk, per-category tally,
// over-budget report — is NOT here. It lives in ci_budget_gate.go, gate-neutral,
// because `llz ci core-surface` (ADR 0014) runs on the same engine pointed the
// opposite way. Anything added here should be specific to THIS gate; a change
// that would serve both belongs in the engine.

import (
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

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

// untestableRemedy is the untestable-loc gate's guidance. `{config}` expands to
// the config path — a placeholder rather than a %s verb because the alternative
// remedy comes from YAML, where a stray % would turn into a format error.
const untestableRemedy = "Move the logic into unit-tested Go " +
	"(tools/cmd/llz), or — for genuine install/glue with no logic — add the file to " +
	"`exclude:` in {config} with a justification. Do NOT raise the budget to make this pass."

func runUntestableLOC(root, configPath string, verbose bool) error {
	return runBudgetGate("untestable-loc", root, configPath, verbose, untestableRemedy)
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
	return countLinesSkippingComments(content, "#")
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

// countMakefileRecipeLines counts logical lines of shell embedded in Makefile
// recipe bodies (tab-indented lines under a rule) and in `define … endef` macro
// bodies, which expand to recipe shell just the same.
//
// This category exists because the Makefile was invisible to every other counter:
// its recipes are neither a workflow `run:` block, a standalone script, nor a
// Terraform provisioner heredoc. That left a hole in the ratchet where shell
// evicted from a workflow INTO a Makefile recipe scored as a reduction, which is
// the opposite of what this gate is for.
//
// Counting mirrors countRunBlockLines so the two are directly comparable and
// neither penalizes a conversion:
//   - blank lines and whole-line `#` comments are skipped;
//   - a backslash-continued command counts once (wrapping a long tool call
//     across physical lines is not a cost);
//   - recipe-line prefixes (`@`, `-`, `+`) are stripped before inspection;
//   - a recipe whose whole body is ONE logical line is tool-invocation glue —
//     exactly the shape `$(call LLZ_CI,<verb>)` takes — and is not counted, the
//     same rule single-line `run:` gets. Converting a multi-line recipe into one
//     `llz ci` call therefore drops it from the tally entirely.
func countMakefileRecipeLines(content string) int {
	lines := strings.Split(content, "\n")
	total := 0
	for i := 0; i < len(lines); i++ {
		var body []string
		switch {
		case strings.HasPrefix(lines[i], "define "):
			// Macro body: everything up to `endef`. Expands into recipes, so its
			// shell is as untestable as a rule's.
			for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "endef"; i++ {
				body = append(body, lines[i])
			}
		case strings.HasPrefix(lines[i], "\t"):
			// A rule's recipe: the run of tab-indented lines. Blank lines do not
			// end the run — a recipe may be visually grouped, and two recipes are
			// always separated by a (non-tab, non-blank) target line, so absorbing
			// blanks can never merge two rules into one body.
			for ; i < len(lines) && (strings.HasPrefix(lines[i], "\t") || strings.TrimSpace(lines[i]) == ""); i++ {
				body = append(body, lines[i])
			}
			i-- // re-examine the first non-recipe line in the outer loop
		default:
			continue
		}
		total += countRecipeBodyLines(body)
	}
	return total
}

// embeddedShellKeyRE matches a YAML key that opens a block scalar (`key: |`,
// `key: |-`, `key: >`, `key: |2`, …) — the carrier for a script embedded in
// YAML. Group 1 is the key's indent, group 2 the key itself.
var embeddedShellKeyRE = regexp.MustCompile(`^(\s*)([^\s#][^:]*):\s*[|>][0-9]*[+-]?\s*$`)

// shellShebangRE recognises a shell shebang as a block's first body line — the
// language-agnostic signal that the block holds a shell script (vs config data,
// prose, or another language). Covers sh/bash/dash/ash, plain or via env.
var shellShebangRE = regexp.MustCompile(`^#!.*\b(?:ba|da|a)?sh\b`)

// countEmbeddedShellLines counts non-blank, non-comment logic lines of shell
// embedded in a YAML block scalar — a ConfigMap `data` entry (`relabel.sh: |`)
// or an Argo `script.source: |`. A block counts as shell when its key ends in
// `.sh`/`.bash` OR its first body line is a shell shebang, so YAML/config block
// scalars and other languages are left out. Counting mirrors countScriptLines
// (the shebang, being a `#` comment, is not counted), scoped to the block body —
// delimited by indentation like countRunBlockLines.
func countEmbeddedShellLines(content string) int {
	lines := strings.Split(content, "\n")
	total := 0
	for i := 0; i < len(lines); i++ {
		m := embeddedShellKeyRE.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		indent := len(m[1])
		key := strings.TrimSpace(m[2])
		keyIsShell := strings.HasSuffix(key, ".sh") || strings.HasSuffix(key, ".bash")

		// Confirm shell via the first non-blank body line (unless the key already
		// settles it). A body line at/under the key's indent => empty block.
		isShell := keyIsShell
		if !isShell {
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) == "" {
					continue
				}
				if lineIndent(lines[j]) <= indent {
					break
				}
				isShell = shellShebangRE.MatchString(strings.TrimSpace(lines[j]))
				break
			}
		}
		if !isShell {
			continue
		}

		for i++; i < len(lines); i++ {
			l := lines[i]
			if strings.TrimSpace(l) == "" {
				continue
			}
			if lineIndent(l) <= indent {
				i-- // re-examine in the outer loop (could open another block)
				break
			}
			if strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			total++
		}
	}
	return total
}

// countRecipeBodyLines returns the logical-line count of one recipe body, or 0
// when the body is a single logical line (glue — see countMakefileRecipeLines).
func countRecipeBodyLines(body []string) int {
	n := 0
	prevContinues := false
	for _, l := range body {
		s := strings.TrimSpace(l)
		if s == "" {
			continue
		}
		// Strip make's recipe-line prefixes (@ silent, - ignore-errors, + always-run)
		// so `@# comment` and `@echo` are classified on their shell content.
		s = strings.TrimLeft(s, "@-+")
		s = strings.TrimSpace(s)
		// Not logic: a `#` comment, or a line whose entire effect is printing
		// literal text. See isDocPrintLine.
		free := s == "" || strings.HasPrefix(s, "#") || isDocPrintLine(s)
		if !free && !prevContinues {
			n++
		}
		prevContinues = !free && strings.HasSuffix(s, `\`)
	}
	if n <= 1 {
		return 0
	}
	return n
}

// isDocPrintLine reports whether a recipe line's ENTIRE effect is writing literal
// text to stdout — the `@echo "…"` walls a `help:` target is made of.
//
// WHY THIS IS NOT LOGIC. The budget exists because "decision-making logic belongs
// in unit-tested Go, not in CI shell" (.untestable-budget.yaml). A line that
// prints a fixed string makes no decision, reads no state, and writes nothing a
// later step can consume. Counting it charged DOCUMENTATION at the same rate as
// shell logic, so `make help` — the one place a new target is discoverable — cost
// a line of budget per target. That is an incentive pointing the wrong way: it
// made the cheapest way to stay under the ceiling "do not document the target",
// and this repo hit it adding two guards.
//
// It is deliberately narrow, because "it starts with echo" is a loophole:
// `echo x > generated.conf` writes a file and `echo $(date)` runs a command.
// Quoted spans are removed FIRST, then the remainder must be exactly the command
// word — so any operator that survives quoting (`>`, `>>`, `|`, `;`, `&&`, a
// second argument) disqualifies the line. Command substitution is rejected on
// what the SHELL would see: `$(X)` in a Makefile is a make expansion resolved
// before the shell exists, while `$$(X)` and a backtick are the shell's own.
func isDocPrintLine(s string) bool {
	rest, quoted := stripQuotedSpans(s)
	if !docPrintCmdRE.MatchString(rest) {
		return false
	}
	// Substitution inside the quoted text still runs a command, so the line is
	// not merely printing. `\$$` is an escaped dollar (literal text) and stays.
	return !shellSubstRE.MatchString(quoted)
}

var (
	// The command word alone must survive quote-stripping: `echo` / `printf`, and
	// nothing else. A trailing operator or an unquoted argument leaves residue
	// here and disqualifies the line.
	docPrintCmdRE = regexp.MustCompile(`^(echo|printf)\s*$`)
	// Shell command substitution as it appears in MAKEFILE SOURCE: `$$(…)` (make
	// collapses `$$` to the `$` the shell then acts on) or a backtick. A
	// backslash-escaped `\$$(` is literal text to the shell and must NOT match.
	shellSubstRE = regexp.MustCompile("(^|[^\\\\])\\$\\$\\(|`")
)

// stripQuotedSpans removes '…' and "…" spans from a shell line, returning the
// unquoted remainder and the concatenated quoted content. Splitting them is what
// lets the caller apply different rules to each: an operator inside quotes is
// literal text (`echo "a; b"` prints a semicolon), while the same character
// outside them is shell syntax.
//
// Backslash escapes are honoured inside double quotes only, matching the shell.
func stripQuotedSpans(s string) (rest, quoted string) {
	var out, q strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'':
			for i++; i < len(s) && s[i] != '\''; i++ {
				q.WriteByte(s[i])
			}
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					q.WriteByte(s[i])
					i++
				}
				q.WriteByte(s[i])
			}
		default:
			out.WriteByte(c)
		}
	}
	return strings.TrimSpace(out.String()), q.String()
}

func lineIndent(l string) int {
	return len(l) - len(strings.TrimLeft(l, " "))
}
