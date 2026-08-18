package promote

// stages.go reads the stages back OUT of an existing promote.yml so the pipeline
// can be checked against the DEPLOYMENTS THE INSTANCE DECLARES — not only against
// the ranks it was generated from.
//
// Why this exists: the rank comparison alone cannot see the failure it was
// supposed to prevent. A dispatched promote.yml naming dev → staging → prod on an
// instance whose spec declares only `prod` failed three jobs deep inside
// `llz render` ("env \"dev\" not in spec"), and `llz env pipeline --check` had
// reported "in sync" on the same tree minutes earlier — because with fewer than
// two ranked deployments PlanWorkflow abstains, and abstaining was reported as
// agreement. The ranks and the file agreed with each other; neither agreed with
// the spec. Two values consistent with each other are not two correct values, so
// the gate now asks the question whose answer the pipeline actually depends on:
// does every stage name a deployment that exists?
//
// Parsed as YAML rather than pattern-matched: "which deployment does this stage
// apply, and is this job a stage at all" is a structural question about the
// document, and a regex over `region:` lines would also match the input
// DECLARATION in a reusable body and any commented-out example.

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// StageRef is one promote stage as the on-disk workflow declares it: the job id,
// the deployment its `region:` input names, what it does there, and what it waits
// for. Env is "" when the stage declares no region at all — a distinct defect from
// naming the wrong one, and reported as such, because "could not tell" and "told us
// something false" send a reader to different places.
type StageRef struct {
	Job    string
	Env    string
	Action string   // the `action:` input — only `apply` promotes anything
	Needs  []string // the job ids this stage waits for; `needs:` IS the green gate
}

// Unresolvable reports that a value is a GitHub expression rather than a literal,
// so llz cannot say what it will be at run time.
//
// IT IS NOT A DEFECT AND MUST NEVER BE TREATED AS ONE. `region: ${{ inputs.target }}`
// is legal YAML, legal GHA, and the natural shape of a hand-maintained promote.yml.
// The stage check compares names against the spec, and an expression has no name to
// compare — so the honest answer is "could not tell", exactly as for a stage with no
// `region:` at all. An earlier cut of namesMissingDeployment tested `Env != ""` and
// so read every expression as "names a deployment that does not exist", which made
// `llz env pipeline` DELETE the operator's pipeline as the remedy `--check` printed:
// two stages in, zero stages out. That is the third time this change generalised a
// remedy into a rule and destroyed something; TestExpressionRegionIsNotOverwritten
// pins it.
func Unresolvable(v string) bool { return strings.Contains(v, "${{") }

// promoteFile is the sliver of a promote.yml this check reads. Everything else in
// the document (triggers, concurrency, permissions) is deliberately not modelled:
// the question is only which jobs are stages, what each applies, and in what order.
type promoteFile struct {
	Jobs map[string]promoteJob `yaml:"jobs"`
}

type promoteJob struct {
	Uses  string    `yaml:"uses"`
	Needs needsList `yaml:"needs"`
	With  struct {
		Region string `yaml:"region"`
		Action string `yaml:"action"`
	} `yaml:"with"`
}

// needsList decodes GitHub's `needs:`, which is a scalar for one dependency and a
// sequence for several. Both forms are idiomatic and the generated file uses the
// scalar one, so a decoder that handled only sequences would read every generated
// pipeline as unchained.
type needsList []string

func (n *needsList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		*n = needsList{s}
		return nil
	}
	var many []string
	if err := node.Decode(&many); err != nil {
		return err
	}
	*n = many
	return nil
}

// isStage reports whether a job is a promotion stage — i.e. it calls the
// llz-terraform reusable body. Matches BOTH forms: the vendored local
// `./.github/workflows/llz-terraform.yml` every ADR-0003 instance renders, and the
// legacy cross-repo `<org>/lke-landing-zone/.github/workflows/llz-terraform.yml@<ref>`
// an older instance still carries. Any other job (the preflight, an operator's own
// notification job) is not a stage and is not checked.
func (j promoteJob) isStage() bool {
	return strings.Contains(j.Uses, "llz-terraform.yml")
}

// workflowStages returns every promotion stage declared in a promote.yml body, in
// job-id order. A body that does not parse is an ERROR, never an empty result: an
// unreadable pipeline is exactly what a broken one looks like, and reporting "no
// stages found" for it would launder a malformed file into a passing gate.
func workflowStages(body []byte) ([]StageRef, error) {
	var f promoteFile
	if err := yaml.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("parse promote.yml: %w", err)
	}
	jobs := make([]string, 0, len(f.Jobs))
	for id := range f.Jobs {
		jobs = append(jobs, id)
	}
	sort.Strings(jobs)
	out := []StageRef{}
	for _, id := range jobs {
		j := f.Jobs[id]
		if !j.isStage() {
			continue
		}
		out = append(out, StageRef{Job: id, Env: j.With.Region, Action: j.With.Action, Needs: j.Needs})
	}
	return out, nil
}

// promotingStages narrows a stage list to the ones that actually promote: `action:
// apply`, or an expression llz cannot resolve (which might be an apply, and a check
// that guesses "no" would let the thing it is looking for through).
//
// WHY THE NARROWING EXISTS: a `plan`-only preview job calls the same reusable body,
// so it counted as a stage — and two jobs of which one is a plan satisfied
// "a pipeline is a chain over at least 2". The name check deliberately still covers
// plan jobs (a plan against a deployment that does not exist fails too); only the
// questions about the SHAPE of the promotion — is there one, is it ordered — are
// asked of the applies.
func promotingStages(stages []StageRef) []StageRef {
	out := []StageRef{}
	for _, s := range stages {
		if s.Action == "apply" || Unresolvable(s.Action) {
			out = append(out, s)
		}
	}
	return out
}

// unordered reports that the promoting stages are not sequenced at all: two or more
// applies, and not one of them waits for another. That file is not a promotion
// pipeline — every stage starts at once and prod applies alongside dev.
//
// `needs:` IS THE PRODUCT. promote.yml's own header says the only things it adds
// over dispatching terraform.yml per deployment are ordering and the per-stage green
// gate, and until this check existed the gate measured neither: a two-stage file with
// no `needs:` between them reported "in sync … and every stage names a declared
// deployment" and exited 0. That is the same shape as the failure this whole change
// is about — three stages firing in parallel — with the names all correct.
//
// DELIBERATELY ONLY THE TOTAL CASE. A partial chain (one stage fanning out to two)
// is an operator's choice and stays legal; a hand-maintained file is not required to
// look like the generated one. Zero edges is the case that cannot be a choice,
// because there is nothing left for the file to mean.
func unordered(stages []StageRef) bool {
	promoting := promotingStages(stages)
	if len(promoting) < 2 {
		return false
	}
	// THE EDGE MUST BE BETWEEN TWO APPLIES. Built over every stage instead, ONE edge
	// to a `plan` preview job masked a total absence of order among the applies:
	// dev and prod both `needs: preview`, so both "had" a dependency and the check
	// passed, while dev and prod still started together. The question is whether the
	// promotions are sequenced relative to EACH OTHER, so only those ids count.
	// (The generated entry stage's `needs: llz-preflight` is likewise not an ordering
	// — it is a gate. The chain is proven by stage 2 needing stage 1.)
	ids := make(map[string]bool, len(promoting))
	for _, s := range promoting {
		ids[s.Job] = true
	}
	for _, s := range promoting {
		for _, n := range s.Needs {
			if ids[n] {
				return false
			}
		}
	}
	return true
}

// missingPreflight reports that a file with stages has none of them chained to a job
// that is not itself a stage — so there is no gate in front of the chain.
//
// A PROXY, DELIBERATELY, and the looseness runs in the safe direction. It cannot see
// what a non-stage job DOES: an operator's own gate job satisfies it without running
// `llz env pipeline --check`, and so does an unrelated notification job. The precise
// test — "does a stage need the job id `llz-preflight`" — would nag every operator
// who wrote an equivalent gate under another name, forever, with no way to silence
// it. For something that only ever prints an advisory, missing a file that has SOME
// gate is a better error than badgering one that has a different gate.
//
// REPORTED, NEVER FATAL. Every promote.yml generated before this change lacks the
// preflight, and `llz env pipeline` leaves a valid unranked pipeline alone by design
// (see PlanWorkflow), so those instances would never acquire one — failing them would
// turn "you are missing an improvement" into "your repo is broken", which is not what
// happened. The advisory is the only channel that tells them, because promote.yml is
// `owned` and an upgrade will not deliver the job either.
func missingPreflight(stages []StageRef) bool {
	if len(stages) == 0 {
		return false
	}
	isStageJob := make(map[string]bool, len(stages))
	for _, s := range stages {
		isStageJob[s.Job] = true
	}
	for _, s := range stages {
		for _, n := range s.Needs {
			if !isStageJob[n] {
				return false
			}
		}
	}
	return true
}

// namesMissingDeployment narrows a set of flagged stages to the ones that name a
// deployment which DOES NOT EXIST — excluding the ones flagged because llz could not
// tell what they name at all: no `region:`, or an expression (see Unresolvable).
//
// THE DIFFERENCE DECIDES WHETHER A FILE GETS OVERWRITTEN, so it is the difference
// between fixing an unrunnable file and deleting a working one. All three kinds are
// worth REPORTING, so undeclaredStages flags all three and the message distinguishes
// them. But `region:` is `required: false` on the reusable body and `${{ … }}` is
// ordinary GHA, so both are odd-or-fine rather than impossible — and destroying an
// operator's promote.yml over either is the over-reach that has now twice wiped a
// valid pipeline (unranked stages in review round 2, expression regions in round 4).
// Only a literal name that is not in the spec is unrunnable enough to justify the
// write, because only then does llz know for certain the run would fail.
//
// The Unresolvable arm is belt-and-braces: expression stages are routed to
// Plan.Unresolved and never reach undeclaredStages' output at all. It is repeated
// here because this is the function whose answer authorises a destructive write, and
// that is the one place worth being redundant about.
func namesMissingDeployment(flagged []StageRef) []StageRef {
	out := []StageRef{}
	for _, s := range flagged {
		if s.Env != "" && !Unresolvable(s.Env) {
			out = append(out, s)
		}
	}
	return out
}

// undeclaredStages returns the stages whose `region:` does not name one of the
// declared deployments (plus any stage llz cannot resolve a name for at all).
//
// NOTE the direction of the comparison: the expected set is the DEPLOYMENTS, read
// from the spec, and the thing under test is the WORKFLOW. Deriving the expected
// set from the workflow instead would make it agree with itself by construction —
// which is precisely the shape of the bug this catches.
//
// An instance that declares no deployments at all is not a free pass: every stage
// in the file is then undeclared, and rightly so, because a pipeline over zero
// deployments cannot apply anything. A file with no stages makes no claim and so
// has nothing to falsify — that is the un-configured instance the template ships,
// and it stays green until an operator ranks something.
//
// A stage whose `region:` is an EXPRESSION is returned separately, and is not a
// failure. There is no name to compare, so calling it undeclared asserts a falsehood
// llz has no evidence for — the mirror image of the abstention-as-agreement this gate
// removes, and just as wrong: it would fail such an instance forever with no reachable
// remedy, since no edit makes `${{ inputs.target }}` resolvable at check time. It is
// reported as UNVERIFIED instead, and the passing message stops claiming coverage it
// does not have.
func undeclaredStages(stages []StageRef, declared []string) (bad, unresolved []StageRef) {
	known := make(map[string]bool, len(declared))
	for _, n := range declared {
		known[n] = true
	}
	bad, unresolved = []StageRef{}, []StageRef{}
	for _, s := range stages {
		switch {
		case Unresolvable(s.Env):
			unresolved = append(unresolved, s)
		case s.Env == "" || !known[s.Env]:
			bad = append(bad, s)
		}
	}
	return bad, unresolved
}

// UndeclaredErr renders the failure a reader can act on without opening a cluster
// or diffing two files: which stage, which deployment it asked for, and — the part
// that is not recoverable from the error alone — WHICH DEPLOYMENTS DO EXIST. The
// name being looked for was never there; only the list of names that are there
// says whether the fix is `llz env add` or `llz env pipeline`.
//
// Returns nil when there is nothing wrong, so callers can `if err := …; err != nil`.
func (p Plan) UndeclaredErr() error {
	if len(p.Undeclared) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "promote.yml names %d deployment(s) this instance does not declare", len(p.Undeclared))
	for _, s := range p.Undeclared {
		if s.Env == "" {
			fmt.Fprintf(&b, "\n  stage %q declares no region: input — it applies nothing", s.Job)
			continue
		}
		fmt.Fprintf(&b, "\n  stage %q applies %q", s.Job, s.Env)
	}
	if len(p.Declared) == 0 {
		b.WriteString("\ndeclared deployments: none — this instance has no environments/<env>.yaml yet")
	} else {
		fmt.Fprintf(&b, "\ndeclared deployments: %s", strings.Join(p.Declared, ", "))
	}
	// THE REMEDY HAS TO ACCOUNT FOR THE NEXT COMMAND THE READER RUNS. `llz env add`
	// regenerates promote.yml as its last step, and while ANY stage still names a
	// deployment that does not exist the regeneration replaces the file with the empty
	// placeholder. So an operator told to "create the missing deployment" who has two
	// of them loses the pipeline on the first add — having done exactly as instructed.
	// Naming every missing deployment, and saying what happens in between, is the
	// difference between advice and a trap.
	missing := namesMissingDeployment(p.Undeclared)
	if len(missing) > 1 {
		names := make([]string, 0, len(missing))
		for _, s := range missing {
			names = append(names, s.Env)
		}
		fmt.Fprintf(&b, "\nfix: create ALL %d missing deployments (`llz env add %s`) — while any one of them"+
			"\n     is still absent, regenerating replaces this file with the empty placeholder", len(missing), strings.Join(names, "`, `llz env add "))
	} else {
		b.WriteString("\nfix: `llz env add <env> …` to create the missing deployment")
	}
	b.WriteString("\n     or set promotionRank on the deployments you DO have and run `llz env pipeline`," +
		"\n     which regenerates the chain from those")
	return fmt.Errorf("%s", b.String())
}

// Applied returns the Plan as it stands AFTER Content has been written to disk:
// the stages are re-read from the bytes just written, and Undeclared is gone
// because the generated stages come from the declared deployments by construction.
//
// IT RE-READS RATHER THAN CLEARING. The first cut of this cleared Undeclared and
// left Stages holding what was on disk BEFORE the write, so
// `llz env pipeline --require-pipeline` regenerated a two-stage pipeline and then
// exited 1 reporting "declares 0 stage(s)" about the file it had just written.
// Parsing the new bytes with the same predicate the checker uses cannot drift out
// of step with them that way.
func (p Plan) Applied() Plan {
	// The file now matches what the ranks render, so it is no longer drift — and
	// leaving Changed set would make RunnableErr report "out of date" immediately
	// after the write that brought it up to date.
	p.Changed = false
	// A parse failure here is unreachable — Content came from this package's own
	// renderer — and if it ever were not, the zero stage list is the honest answer
	// for bytes we cannot read.
	p.Stages, _ = workflowStages([]byte(p.Content))
	// RE-DERIVED from the written stages, not cleared. Both fields describe the file,
	// and the whole reason this method re-reads is that a hand-cleared field went out
	// of step with the bytes on disk once already.
	p.Undeclared, p.Unresolved = undeclaredStages(p.Stages, p.Declared)
	return p
}

// RunnableErr is the composed question the CLI actually asks: can the promote.yml
// on disk be dispatched? Always the stage-vs-spec check; additionally "is there a
// pipeline at all" when the caller passes requirePipeline.
//
// Composed HERE rather than as two `if` blocks in internal/cli because the choice
// between the two contexts is a judgement about promotion, not command wiring —
// and because the wiring layer is budgeted (ADR 0014) while judgement in an
// extension package is exactly where this repo wants it.
func (p Plan) RunnableErr(requirePipeline bool) error {
	if err := p.UndeclaredErr(); err != nil {
		return err
	}
	// THE ORDER IS NOT ARBITRARY. Each arm is the most specific true statement
	// available at that point:
	//
	//   undeclared  — names a deployment that does not exist. Most specific: it
	//                 says WHICH one, and the remedy differs from the others'.
	//   drift       — regenerable. `llz env pipeline` fixes it, so say that — and
	//                 regenerating fixes the ordering too, which is why drift is
	//                 reported ahead of it rather than after.
	//   unordered   — has stages, and no order over them. Not regenerable from
	//                 ranks (there are none), so it is the operator's edit to make.
	//   no pipeline — nothing to run and nothing to regenerate from.
	//
	// With NoPipelineErr ahead of drift, a placeholder on disk plus two ranked
	// deployments reported "no promotion pipeline to run" and printed a remedy the
	// operator had ALREADY done — while suppressing the accurate one, which was
	// simply to regenerate.
	if p.Changed {
		return fmt.Errorf("promote.yml is out of date with the promotion_rank ordering — run `llz env pipeline` and commit the result")
	}
	if err := p.UnorderedErr(); err != nil {
		return err
	}
	if requirePipeline {
		return p.NoPipelineErr()
	}
	return nil
}

// UnorderedErr reports a promote.yml with two or more applying stages and no `needs:`
// edge anywhere between them. Fatal in BOTH contexts, unlike NoPipelineErr: "no
// pipeline yet" is a state every instance starts in, but "a pipeline with no order"
// is never a state anything starts in — it takes an edit to produce, and what it
// produces is a simultaneous apply to every deployment named in the file.
func (p Plan) UnorderedErr() error {
	if !unordered(p.Stages) {
		return nil
	}
	var b strings.Builder
	applies := promotingStages(p.Stages)
	// "another APPLYING stage", because the listed `needs:` may well be non-empty —
	// a shared `plan` preview job is the case that produced this arm's first false
	// negative. A message that says "not one of them needs another" above a line
	// reading `needs: [preview]` contradicts its own evidence and sends the reader
	// looking for a different bug.
	fmt.Fprintf(&b, "promote.yml declares %d applying stage(s) and no order over them — not one of them `needs:` another APPLYING stage", len(applies))
	for _, s := range applies {
		fmt.Fprintf(&b, "\n  stage %q applies %q with needs: %v", s.Job, s.Env, []string(s.Needs))
	}
	b.WriteString("\nthis is not a promotion: every stage starts at once, so the last deployment applies" +
		"\nalongside the first rather than after it — the ordering IS what promote.yml adds" +
		"\nover dispatching terraform.yml per deployment" +
		"\nfix: chain each stage to the one before it (`needs: <previous job>`), or set" +
		"\n     promotionRank on the deployments and run `llz env pipeline` to generate the chain")
	return fmt.Errorf("%s", b.String())
}

// CheckReport is everything `llz env pipeline` has to SAY about a plan, and the
// error it has to return — the whole verdict in one call, so the wiring layer prints
// lines it did not compose.
//
// IT LIVES HERE FOR THE SAME REASON RunnableErr DOES. Which sentence is true of a
// tree is a judgement about promotion, and this file is where that judgement is under
// unit test; internal/cli is budgeted precisely to stop it accumulating there. Written
// as three `if` blocks in the command it cost eleven lines of the budget and put the
// ordering — advisories before the success line, nothing at all before an error — in
// the one place no test reads.
//
// THE PATH GUARD IS FIRST, and that is the point of it. Path is empty on a
// template-repo checkout, where PlanWorkflow reads nothing at all, so every verdict
// below is about a comparison that never ran. With the guard merely in front of the
// success line, `--check --require-pipeline` at the repo root failed with "this
// instance declares no deployments yet" — this gate's own original sin wearing the
// fix as a hat.
func (p Plan) CheckReport(check, requirePipeline bool) ([]string, error) {
	if p.Path == "" {
		if check {
			return []string{"promote.yml: not an instance checkout — nothing to check."}, nil
		}
		return nil, nil
	}
	if err := p.RunnableErr(requirePipeline); err != nil {
		return nil, err
	}
	out := p.Advisories()
	if check {
		out = append(out, p.passedMsg())
	}
	return out, nil
}

// passedMsg is the green verdict, and it may only claim what was actually compared.
//
// A SENTENCE IS AN ASSERTION. "every stage names a declared deployment, and the
// stages are ordered" was printed verbatim for a promote.yml with NO STAGES — the
// state every fresh instance is in, so the state the PR gate prints on most often —
// asserting two comparisons that had nothing to run against. That is the same
// abstention-dressed-as-agreement this whole change removes, committed by the line
// announcing the fix. Each clause is now conditional on the check that earns it.
func (p Plan) passedMsg() string {
	var b strings.Builder
	b.WriteString("promote.yml is in sync with the promotion_rank ordering")
	switch {
	case len(p.Stages) == 0:
		b.WriteString(", and declares no stages (nothing to promote yet)")
		return b.String()
	case len(p.Unresolved) == len(p.Stages):
		// EVERY stage is an expression, so the name check compared nothing at all —
		// and on a tree with no declared deployments it would otherwise announce that
		// every stage names one of them. Claiming a comparison over an empty set is
		// the same defect as claiming it over no stages, one level finer.
		b.WriteString(", and names no deployment llz can check (every stage is an expression)")
		return b.String()
	default:
		b.WriteString(", every resolvable stage names a declared deployment")
	}
	if len(promotingStages(p.Stages)) >= 2 {
		b.WriteString(", and the stages are ordered")
	}
	b.WriteString(".")
	if n := len(p.Unresolved); n > 0 {
		// The claim above must not cover stages llz could not resolve a name for.
		fmt.Fprintf(&b, " (%d stage(s) unverified — see above)", n)
	}
	return b.String()
}

// Advisories are true statements about the on-disk pipeline that are NOT failures:
// things llz could not verify, and improvements the file has not picked up. The CLI
// prints them alongside a passing `--check` so a green exit never doubles as a claim
// that everything was checked.
//
// WHY NOT ERRORS. Both arms describe files that run correctly today. Failing on an
// expression `region:` would fail an instance forever with no reachable remedy;
// failing on a missing preflight would fail every promote.yml generated before this
// change — and since promote.yml is `owned`, an upgrade will never add the job for
// them, so the failure would be unearned twice over. Advisories are the only channel
// that reaches those instances at all.
func (p Plan) Advisories() []string {
	var out []string
	for _, s := range p.Unresolved {
		out = append(out, fmt.Sprintf("promote.yml: stage %q applies %q — an expression, so llz cannot check it against the spec. UNVERIFIED, not accepted: whatever it resolves to at run time must be a declared deployment.", s.Job, s.Env))
	}
	if missingPreflight(p.Stages) {
		out = append(out, "promote.yml: no stage chains from a non-stage job, so this pipeline has no preflight — a dispatch runs with nothing checking it first. "+
			"Instances generated before the preflight existed are in this state, and `llz upgrade` will not add it (promote.yml is `owned`). "+
			"To adopt it: set promotionRank on the deployments you chain and run `llz env pipeline`, or copy the llz-preflight job from docs/environments-and-promotion.md.")
	}
	return out
}

// NoPipelineErr reports that there is no pipeline to run — fewer than two stages
// in the file being dispatched.
//
// SEPARATE FROM UndeclaredErr BECAUSE THE TWO CONTEXTS WANT OPPOSITE ANSWERS, and
// collapsing them is what produced the failure this whole change is about. On a
// pull request, "this instance has not built a pipeline yet" is a normal state and
// must not fail the build — every instance is in it until somebody ranks two
// deployments. At DISPATCH it is the whole failure: the operator pressed Run
// workflow on a promotion that promotes nothing. So the PR gate calls
// UndeclaredErr only, and promote.yml's preflight asks for this one too
// (`--require-pipeline`).
//
// Returns nil when a pipeline exists, so callers can `if err := …; err != nil`.
func (p Plan) NoPipelineErr() error {
	// COUNTED OVER THE APPLYING STAGES, not every job that calls the reusable body.
	// A `plan`-only preview job is a stage for the name check and not a promotion, so
	// counting it here let "one apply plus one plan" satisfy "a chain over at least 2".
	applies := promotingStages(p.Stages)
	if len(applies) >= 2 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "no promotion pipeline to run — promote.yml declares %d applying stage(s), and a pipeline is a chain over at least 2", len(applies))
	if len(p.Stages) > len(applies) {
		fmt.Fprintf(&b, "\n(%d job(s) call llz-terraform.yml without `action: apply` — those promote nothing)", len(p.Stages)-len(applies))
	}
	if len(p.Declared) == 0 {
		b.WriteString("\nthis instance declares no deployments yet — start with `llz env add <env> …`")
	} else {
		fmt.Fprintf(&b, "\ndeclared deployments: %s", strings.Join(p.Declared, ", "))
	}
	b.WriteString("\nto build one: set promotionRank (ascending: 1, 2, 3 …) on at least two deployments" +
		"\n              in environments/<env>.yaml, then run `llz env pipeline` and commit the result" +
		"\nto apply ONE deployment without a pipeline, dispatch .github/workflows/terraform.yml instead")
	return fmt.Errorf("%s", b.String())
}
