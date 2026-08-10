package budget

// count.go — the per-language logic-line counters the gates tally with.
//
// Each is a pure function over file content, and that purity is the reason they
// live here rather than beside a cobra command: the budgets they feed were first
// set by one-off measurement scripts, and the counting rules deliberately mirror
// those scripts so the numbers stay reproducible. A rule that changes silently
// re-baselines every budget in the repo.
//
//	countRunBlockLines             every non-blank, non-comment line inside a
//	                               `run:` block of a workflow / composite action
//	countScriptLines               non-blank, non-comment lines of *.sh / *.py
//	countTerraformProvisionerLines bash inside `command = <<EOT … EOT` heredocs
//	countMakefileRecipeLines       shell in recipe bodies and `define … endef`
//	countEmbeddedShellLines        shell inside a YAML block scalar
//	countGoLogicLines              non-blank, non-`//` lines of Go (ADR 0014)
//
// Two rules recur and are worth stating once. A SINGLE logical line is glue, not
// logic — it is exactly what a converted step looks like (`llz ci <verb>`), so
// counting it would penalise the conversions the untestable-loc gate exists to
// encourage. And a backslash-continued command counts once, so wrapping a long
// tool call across physical lines costs nothing.

import (
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/shquote"
)

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
			// Single-line command (`run: llz ci <verb>`) is tool-invocation glue,
			// not embedded logic — it's exactly what a converted step looks
			// like, so counting it would penalize the conversions this gate
			// exists to encourage. Only multi-line `run:` blocks (which hold
			// real logic) are counted.
			continue
		}
		// Block scalar: count LOGICAL lines of the body until the indentation
		// returns to <= the run: directive's own indent. Backslash-continued
		// commands count once — a `llz ci <verb> --a \ --b \ --c` invocation that
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
	rest, quoted := shquote.StripSpans(s)
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

func lineIndent(l string) int {
	return len(l) - len(strings.TrimLeft(l, " "))
}

// countGoLogicLines counts non-blank, non-comment lines of Go — the counter
// behind the core-surface budget (ADR 0014).
//
// It deliberately does NOT track /* … */ block comments. Go in this repo is
// documented with `//` (godoc's form), so a block-comment state machine would
// buy nothing — and it would MISFIRE, because `/*` appears here almost only
// inside string literals: glob patterns (`"tools/cmd/llz/*.go"`,
// `"terraform-iac-bootstrap/*/.terraform.lock.hcl"`) and regexes. A naive
// scanner treats the first of those as an unterminated comment and swallows the
// rest of the file, which undercounts by more than half. Matching the sibling
// counters' rule — blank, or the line STARTS with the comment marker — has
// neither failure mode, and leaves `x := 1 // note` counted as the logic it is.
func countGoLogicLines(content string) int {
	return countLinesSkippingComments(content, "//")
}
