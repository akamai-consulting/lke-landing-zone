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
var runStep = regexp.MustCompile(`(?m)^(\s*)-?\s*run:\s*(\|[-+>]*|>[-+]*)?[ \t]*(.*)$`)

// ghExpr matches a GitHub Actions `${{ … }}` expression, which is substituted
// before the shell ever sees it and so is never a command word.
var ghExpr = regexp.MustCompile(`\$\{\{[^}]*\}\}`)

// commandWords returns the leading word of every command position in a shell
// snippet: the start of the script, and after each `|`, `&&`, `||`, `;`.
// Continuations, comments, env-var prefixes (`FOO=bar cmd`) and `sudo` are
// stepped over so the real command word is what comes back.
func commandWords(script string) [][]string {
	var out [][]string
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
	for _, w := range rest {
		if strings.HasPrefix(w, "-") {
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
