package cli

// delivered_commands_test.go — a delivered workflow may only shell out to a
// command the instance actually has.
//
// ────────────────────────────────────────────────────────────────────────────
// THE SPLIT CONTRACT. instance-template/.github/ decides WHAT an instance's CI
// runs; this Go module decides WHAT `llz` PROVIDES. Both were correct the day
// they were written, and nothing connected them.
//
// The tflint and checkov checks began life as targets in this repo's root
// Makefile and were ported into the binary (verbs/lint/lint.go says so in its
// header) precisely so they would travel with the image instead of via `copier
// update`. The port updated the producer and not the call site: the delivered
// llz-terraform.yml went on running `make tf-lint` / `make checkov`, and
// instance-template ships no Makefile. Every instance scaffolded since had two
// CI gates that could only ever fail with
//
//	make: *** No rule to make target 'tf-lint'.  Stop.
//
// WHY IT SURVIVED SO LONG, which is the part worth designing against. Both jobs
// are gated `if: github.event_name == 'pull_request'` behind a `paths:` filter,
// and the example instance is driven entirely by workflow_dispatch and schedule
// — 60 runs, zero pull_request events. A gate nothing triggers is
// indistinguishable from a gate that passes. So this test does not wait for the
// jobs to run: it resolves every delivered invocation at PR time, here.
//
// DOCTRINE (docs/e2e-gates.md). The consumer's REAL predicate is the cobra tree
// this binary actually registers, walked below — not a restated list of command
// names, which would be a test of the test and would drift the same way the
// Makefile did. Renaming or unregistering a command a delivered workflow calls
// fails here, including the hidden `llz check`, whose hiddenness is a help-text
// choice and not a licence to delete it.
//
// SCOPE, STATED. Only the leading command word of each `run:` line is examined,
// and for `llz` only the subcommand PATH (resolution stops at the first flag or
// `${{ }}` expression, which are arguments rather than command words). Shell
// builtins and the other binaries the ci images carry are out of scope — the
// image contract is version-pins-check's job. What is in scope is the two
// spellings that have actually broken: a tool the instance does not ship, and an
// llz subcommand that does not exist.
// ────────────────────────────────────────────────────────────────────────────

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// deliveredCITrees are the delivered directories whose `run:` steps execute
// inside an instance: the workflow bodies and the composite actions they drive.
// Both are `managed`, so both ship verbatim and neither is an operator's to fix.
var deliveredCITrees = []string{
	"../../../instance-template/.github/workflows",
	"../../../instance-template/.github/actions",
}

// runStep captures one `run:` step body — either the inline form (`run: cmd`) or
// a block scalar, whose body is the following more-indented lines.
//
// EVERY GAP IS [ \t] AND NOT \s, WHICH IS THE DIFFERENCE BETWEEN A GATE AND A
// GATE-SHAPED NO-OP. `\s` matches a newline, so with `^(\s*)` the match could
// start on a BLANK line and swallow `\n` plus the next line's indentation into
// the indent group. runScripts then looks for body lines beginning with
// indent+" " — an indent that starts with a newline matches nothing, so every
// line of the block scalar is dropped and the step's commands are never examined.
// One blank line above a `run:` was enough to silently exempt it. No delivered
// file has that shape today, which is precisely why it had to be pinned rather
// than noticed: see TestRunStepSurvivesABlankLineBeforeRun.
var runStep = regexp.MustCompile(`(?m)^([ \t]*)-?[ \t]*run:[ \t]*(\|[-+>]*|>[-+]*)?[ \t]*(.*)$`)

// ghExpr matches a GitHub Actions `${{ … }}` expression, which is substituted
// before the shell ever sees it and so is never a command word.
var ghExpr = regexp.MustCompile(`\$\{\{[^}]*\}\}`)

// ghExprToken is what an expression collapses to. It is ONE word on purpose:
// `strings.Fields` would otherwise split `${{ inputs.ref }}` into three (`${{`,
// `inputs.ref`, `}}`), and three phantom words in argument position is how a
// perfectly correct call site gets judged against cobra's arity and fails. The
// substitution happens before any splitting, so nothing downstream ever sees the
// spaces inside an interpolation.
const ghExprToken = "__GH_EXPR__"

// runScripts returns the shell body of every `run:` step in a workflow/action
// file, inline and block-scalar alike.
//
// Extracted from the walk below so the extraction can be TESTED, which is the
// only way a bug in it is ever visible: this gate's failure mode is silence —
// a step it cannot see is a step it reports nothing about, indistinguishable
// from a step that is fine.
func runScripts(body string) []string {
	var out []string
	for _, m := range runStep.FindAllStringSubmatchIndex(body, -1) {
		indent := body[m[2]:m[3]]
		script := body[m[6]:m[7]]
		// Group 2 is the block indicator; it is absent (-1) for the inline
		// `run: cmd` form, where the script is already the rest of the line.
		if m[4] >= 0 && body[m[4]:m[5]] != "" { // block scalar: take the more-indented lines
			rest := body[m[7]:]
			var lines []string
			for _, ln := range strings.Split(rest, "\n")[1:] {
				if strings.TrimSpace(ln) != "" && !strings.HasPrefix(ln, indent+" ") {
					break
				}
				lines = append(lines, ln)
			}
			script = strings.Join(lines, "\n")
		}
		out = append(out, script)
	}
	return out
}

// commandWords returns the leading word of every command position in a shell
// snippet: the start of the script, and after each `|`, `&&`, `||`, `;`.
// Continuations, comments, env-var prefixes (`FOO=bar cmd`) and `sudo` are
// stepped over so the real command word is what comes back.
func commandWords(script string) [][]string {
	var out [][]string
	// Collapse every `${{ … }}` FIRST — before the chunk split and before any
	// field split — so an interpolation is one opaque word wherever it lands.
	script = ghExpr.ReplaceAllString(script, ghExprToken)
	for _, chunk := range regexp.MustCompile(`\|\||&&|[|;\n]`).Split(script, -1) {
		chunk = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(chunk), "\\"))
		if chunk == "" || strings.HasPrefix(chunk, "#") {
			continue
		}
		fields := strings.Fields(chunk)
		for len(fields) > 0 && (strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") || fields[0] == "sudo") {
			fields = fields[1:]
		}
		if len(fields) > 0 {
			out = append(out, fields)
		}
	}
	return out
}

// resolve asks COBRA to resolve argv, rather than reimplementing its lookup.
// Find() is the same traversal `llz` performs at runtime, and ValidateArgs() is
// the same check that turns a stray word into `unknown command "check" for "llz
// lint"` — which is exactly how the broken spelling would fail in CI.
//
// Hand-rolling this was tempting and wrong: an earlier draft walked the children
// itself and treated a stray word after a runnable command as a positional
// argument, so `llz lint check tf-lint` came back clean while the real binary
// rejects it. Reimplementing the consumer's rule reproduces the very drift this
// file exists to catch.
func resolve(root *cobra.Command, argv []string) (*cobra.Command, error) {
	cmd, rest, err := root.Find(argv)
	if err != nil {
		return cmd, err
	}
	if !cmd.Runnable() {
		return cmd, fmt.Errorf("%q is a command group and needs a subcommand", cmd.CommandPath())
	}
	// ARGUMENTS ARE ONLY JUDGED WHEN THEY CAN BE. `rest` is what the SHELL would
	// have split and expanded — this test has neither a shell nor the Actions
	// expression evaluator, so `--title "apply-cluster (LKE-E create) timing"`
	// arrives as five words and `"$ORPHAN_THRESHOLD"` is not an int. Running
	// ValidateArgs over that manufactures failures on eight correct call sites.
	//
	// When the invocation carries NO flags, though, `rest` IS the positional list
	// verbatim, and cobra's own Args validator applies exactly as it would at
	// runtime. That narrow case is the one that matters: it is the shape of
	// `llz lint check tf-lint`, where a stray word rides on a runnable command
	// and only NoArgs catches it.
	// An unexpanded `${{ … }}` is the same case as a flag: this test has no Actions
	// expression evaluator, so it cannot know whether the expression stands for one
	// positional argument, several, or none — and cobra's arity check answers a
	// question that was never asked of it. Judge only what can be judged.
	for _, w := range rest {
		if strings.HasPrefix(w, "-") || strings.Contains(w, ghExprToken) {
			return cmd, nil
		}
	}
	if err := cmd.ValidateArgs(rest); err != nil {
		return cmd, err
	}
	return cmd, nil
}

func TestDeliveredWorkflowCommands(t *testing.T) {
	root := newRootCmd()

	// The consumer side of the `make` half of the contract: a delivered `make`
	// is only legitimate if the scaffold actually ships a Makefile. Asked of the
	// tree rather than asserted as a constant, so shipping one flips this test
	// instead of stranding it.
	_, makefileErr := os.Stat("../../../instance-template/Makefile")
	instanceHasMakefile := makefileErr == nil

	var files []string
	for _, dir := range deliveredCITrees {
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // a tree that does not exist contributes nothing
			}
			if ext := filepath.Ext(p); ext == ".yml" || ext == ".yaml" {
				files = append(files, p)
			}
			return nil
		})
	}
	if len(files) == 0 {
		t.Fatal("no delivered workflow/action files found — this gate would pass having examined nothing")
	}
	sort.Strings(files)

	llzCalls := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		body := string(raw)

		for _, script := range runScripts(body) {
			for _, argv := range commandWords(script) {
				switch argv[0] {
				case "make":
					if !instanceHasMakefile {
						t.Errorf("%s: runs %q, but instance-template ships no Makefile — "+
							"an instance cannot run this. Call the llz verb that carries the check instead.",
							f, strings.Join(argv, " "))
					}
				case "llz":
					llzCalls++
					if _, err := resolve(root, argv[1:]); err != nil {
						t.Errorf("%s: runs %q, which this binary rejects: %v — "+
							"a delivered workflow calls an llz command that does not resolve.",
							f, strings.Join(argv, " "), err)
					}
				}
			}
		}
	}

	// Fail closed on vacuity: the delivered pipeline is built on llz, so finding
	// almost no calls means the extractor broke, not that the tree got clean.
	if llzCalls < 20 {
		t.Fatalf("only %d llz invocations found across %d delivered files — "+
			"the extractor is not seeing the run: steps it is supposed to check", llzCalls, len(files))
	}
	t.Logf("checked %d llz invocations across %d delivered files", llzCalls, len(files))
}

// TestRunStepSurvivesABlankLineBeforeRun pins the extractor against the shape
// that used to disable it silently.
//
// `^(\s*)` could begin its match on a BLANK line and pull the newline plus the
// next line's indentation into the indent group; the block-scalar body is then
// collected by looking for lines starting with indent+" ", which an indent
// containing a newline never matches. Result: an empty script, no command words,
// and a step nobody checked — reported exactly like a step that passed. A single
// blank line above a `run:` was the whole trigger, and no delivered file happens
// to have one, so nothing in the tree would have revealed it.
func TestRunStepSurvivesABlankLineBeforeRun(t *testing.T) {
	// The trigger is a blank line IMMEDIATELY above the `run:` — an ordinary shape
	// in these heavily annotated files, where a step's env: block is separated
	// from its script.
	const body = `jobs:
  j:
    steps:
      - name: inline
        run: llz ci converge
      - name: block after a blank line
        env:
          REGION: e2e

        run: |
          llz check tf-lint
          llz check checkov
`
	scripts := runScripts(body)
	if len(scripts) != 2 {
		t.Fatalf("expected 2 run: steps, got %d: %q", len(scripts), scripts)
	}
	var words []string
	for _, s := range scripts {
		for _, argv := range commandWords(s) {
			words = append(words, strings.Join(argv, " "))
		}
	}
	for _, want := range []string{"llz ci converge", "llz check tf-lint", "llz check checkov"} {
		if !slices.Contains(words, want) {
			t.Errorf("%q was not extracted — the gate would have examined nothing for that step. Got: %q", want, words)
		}
	}
}

// The block-scalar body must still STOP at the next key. An extractor that runs
// past it would drag unrelated YAML into the shell snippet and manufacture
// failures on correct call sites — the opposite over-reach.
func TestRunScriptsStopAtTheNextKey(t *testing.T) {
	const body = `      - name: a
        run: |
          llz ci converge
        env:
          FOO: bar
      - name: b
        run: llz doctor
`
	scripts := runScripts(body)
	if len(scripts) != 2 {
		t.Fatalf("expected 2 run: steps, got %d: %q", len(scripts), scripts)
	}
	if strings.Contains(scripts[0], "FOO") || strings.Contains(scripts[0], "env:") {
		t.Errorf("the block scalar swallowed the following key: %q", scripts[0])
	}
}

// TestDeliveredCommandResolverRejects pins the EXCLUSIONS. A resolver that says
// yes to everything would pass the test above while the delivered tree rotted —
// the failure mode the gate exists to catch.
func TestDeliveredCommandResolverRejects(t *testing.T) {
	root := newRootCmd()

	// The regression itself: the spelling that shipped broken must not resolve.
	if _, err := resolve(root, []string{"lint", "check", "tf-lint"}); err == nil {
		t.Error("`llz lint check tf-lint` resolved, but `check` is a sibling of `lint`, not a child — " +
			"the resolver is not distinguishing command paths")
	}
	for _, argv := range [][]string{
		{"check", "no-such-step"},
		{"ci", "no-such-verb"},
		{"no-such-verb"},
	} {
		if cmd, err := resolve(root, argv); err == nil {
			t.Errorf("llz %s resolved to %q — a nonexistent command must not resolve",
				strings.Join(argv, " "), cmd.CommandPath())
		}
	}

	// And the true positives, so the gate cannot be "fixed" by rejecting
	// everything: these are the call sites llz-terraform.yml now depends on.
	for _, argv := range [][]string{
		{"check", "tf-lint"},
		{"check", "checkov"},
	} {
		if _, err := resolve(root, argv); err != nil {
			t.Errorf("llz %s did not resolve (%v) — the delivered workflow calls it",
				strings.Join(argv, " "), err)
		}
	}
}
