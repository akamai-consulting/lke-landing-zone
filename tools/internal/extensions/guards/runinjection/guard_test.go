package runinjection

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func parse(t *testing.T, src string) workflow {
	t.Helper()
	var w workflow
	if err := yaml.Unmarshal([]byte(src), &w); err != nil {
		t.Fatal(err)
	}
	return w
}

// TestFlagsTheRealInjection injects the exact shape that shipped: a
// workflow_dispatch input interpolated into a run: script, in a job holding a PAT.
func TestFlagsTheRealInjection(t *testing.T) {
	f := Scan("wf.yml", parse(t, `
jobs:
  upgrade:
    steps:
      - name: Run the upgrade
        run: llz upgrade --ref "${{ inputs.ref }}" --commit
`))
	if len(f) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(f))
	}
	if f[0].Expr != "inputs.ref" || f[0].Job != "upgrade" || f[0].Step != "Run the upgrade" {
		t.Errorf("finding must locate the injection precisely, got %+v", f[0])
	}
}

func TestFlagsGithubEvent(t *testing.T) {
	// GitHub's own hardening guidance names this context: a pull-request title or
	// branch name is written by whoever opened it.
	f := Scan("wf.yml", parse(t, `
jobs:
  j:
    steps:
      - run: echo "${{ github.event.pull_request.title }}"
`))
	if len(f) != 1 {
		t.Fatalf("github.event.* must be flagged, got %d finding(s)", len(f))
	}
}

// TestEnvRoutedInputIsNotAFinding is the whole point: the fix must pass.
func TestEnvRoutedInputIsNotAFinding(t *testing.T) {
	f := Scan("wf.yml", parse(t, `
jobs:
  upgrade:
    steps:
      - name: Run the upgrade
        env:
          REF: ${{ inputs.ref }}
        run: llz upgrade --ref "$REF" --commit
`))
	if len(f) != 0 {
		t.Errorf("an input routed through env: is inert and must pass, got %+v", f)
	}
}

// TestDoesNotFlagRepoControlledContexts keeps the rule narrow enough to be kept.
//
// A guard that fires on `vars.TF_IMAGE` would be turned off inside a week, and
// the class it exists for would go back to being invisible. steps/needs outputs
// are a real second tier and are deliberately out of scope for now — see the
// header.
func TestDoesNotFlagRepoControlledContexts(t *testing.T) {
	f := Scan("wf.yml", parse(t, `
jobs:
  j:
    steps:
      - run: |
          echo "${{ vars.TF_IMAGE }}"
          echo "${{ secrets.GITHUB_TOKEN }}"
          echo "${{ github.repository }}"
          echo "${{ matrix.region }}"
          echo "${{ needs.discover.outputs.deployments }}"
          echo "${{ steps.x.outputs.y }}"
`))
	if len(f) != 0 {
		t.Errorf("only inputs.* and github.event.* are in scope; got %+v", f)
	}
}

func TestGithubRepositoryIsNotGithubEvent(t *testing.T) {
	// A prefix check on "github." would swallow github.repository, github.sha and
	// every other server-set value, which is how a rule becomes noise.
	if externallySupplied("github.repository") || externallySupplied("github.sha") {
		t.Error("server-set github.* values are not externally supplied")
	}
	if !externallySupplied("github.event.issue.title") {
		t.Error("github.event.* is externally supplied")
	}
	if !externallySupplied("inputs.ref") {
		t.Error("inputs.* is externally supplied")
	}
}

func TestRunFailsClosedOnAnEmptyCorpus(t *testing.T) {
	// A guard that scanned nothing reports a clean tree it never read — the
	// failure this whole tree refuses.
	var out, errOut bytes.Buffer
	if err := Run(t.TempDir(), &out, &errOut); err == nil {
		t.Error("no workflows must be an error, not a pass")
	}
}

func TestRunFindsAnInjectionOnDisk(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yml"), []byte(`
jobs:
  j:
    steps:
      - name: boom
        run: echo "${{ inputs.thing }}"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := Run(root, &out, &errOut)
	if err == nil {
		t.Fatal("an injection on disk must fail the guard")
	}
	// The remedy is the whole value of the message: an operator who cannot see the
	// fix will reach for a nolint instead.
	if !strings.Contains(errOut.String(), "env:") {
		t.Errorf("the failure must show the env: fix, got:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "boom") {
		t.Errorf("the failure must name the step, got:\n%s", errOut.String())
	}
}

func TestUnparseableWorkflowIsAnError(t *testing.T) {
	// A workflow this guard cannot parse is one it cannot vouch for.
	//
	// THE FIXTURE HAS TO BE OTHERWISE VALID. The first version supplied only the
	// bad file, so Run failed on the empty-corpus vacuity check and the test
	// passed without ever reaching the unmarshal error — swallowing that error
	// left the package green. One unparseable file hiding under a confident
	// "OK — 35 workflow(s)" is exactly the silent under-scanning this guard
	// treats as its worst failure.
	root := t.TempDir()
	dir := filepath.Join(root, ".github", "workflows")
	act := filepath.Join(root, ".github", "actions", "a")
	for _, d := range []string{dir, act} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.yml"),
		[]byte("jobs:\n  j:\n    steps:\n      - run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(act, "action.yml"),
		[]byte("runs:\n  using: composite\n  steps:\n    - run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yml"), []byte("jobs: [oh: dear\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := Run(root, &out, &errOut)
	if err == nil {
		t.Fatalf("an unparseable workflow must be an error, not a silent skip: %s", out.String())
	}
	if strings.Contains(err.Error(), "vacuously") {
		t.Errorf("the failure must be the parse error, not the empty corpus: %v", err)
	}
}

// TestFlagsEveryReferenceInAnExpression pins the gap that let the common case
// through.
//
// The first cut captured only what a `${{ … }}` STARTED with, so an input hiding
// behind anything else passed — and the repo already writes that shape:
// build-images.yml has `${{ github.event_name == 'workflow_dispatch' && inputs.image || 'all' }}`.
func TestFlagsEveryReferenceInAnExpression(t *testing.T) {
	for _, src := range []string{
		`run: echo "${{ vars.X || inputs.y }}"`,
		`run: echo "${{ fromJSON(inputs.payload) }}"`,
		`run: echo "${{ format('{0}', github.event.pull_request.title) }}"`,
		`run: echo "${{ github.event_name == 'workflow_dispatch' && inputs.image || 'all' }}"`,
	} {
		f := Scan("wf.yml", parse(t, "jobs:\n  j:\n    steps:\n      - "+src+"\n"))
		if len(f) == 0 {
			t.Errorf("an input reached a run: script and was missed: %s", src)
		}
	}
}

func TestEventNameIsNotEventPayload(t *testing.T) {
	// `github.event_name` and `github.event_path` are SERVER-SET. A bare
	// HasPrefix("github.event") flags them — a false positive on one of the most
	// common expressions in the tree, and false positives are how a guard gets
	// switched off.
	for _, safe := range []string{"github.event_name", "github.event_path"} {
		if externallySupplied(safe) {
			t.Errorf("%s is set by GitHub, not by a caller", safe)
		}
	}
	for _, unsafe := range []string{"github.event", "github.event.pull_request.title"} {
		if !externallySupplied(unsafe) {
			t.Errorf("%s carries caller-written text", unsafe)
		}
	}
}

func TestFlagsTheBranchNameContexts(t *testing.T) {
	// A pull request's SOURCE BRANCH NAME is attacker-chosen text, and GitHub's
	// hardening guidance cites it right after github.event.*.
	for _, ctx := range []string{"github.head_ref", "github.ref_name"} {
		if !externallySupplied(ctx) {
			t.Errorf("%s is attacker-chosen text", ctx)
		}
	}
	// github.actor is deliberately absent: usernames are [A-Za-z0-9-] and cannot
	// carry shell syntax. Pinned so the omission stays a decision.
	if externallySupplied("github.actor") {
		t.Error("github.actor cannot carry shell syntax; flagging it is noise")
	}
}

func TestScansCompositeActions(t *testing.T) {
	// A composite action holds its steps under `runs:`, not `jobs:` — so scanning
	// only workflows saw 5 of the 11 live interpolations and missed 6, including
	// the one reachable from a workflow_call input (breakglass → cluster-access →
	// fetch-kubeconfig).
	f := Scan("action.yml", parse(t, `
runs:
  using: composite
  steps:
    - name: Fetch kubeconfig
      run: KUBE_REGION="${{ inputs.region }}"
`))
	if len(f) != 1 {
		t.Fatalf("a composite action's run: block is the same hazard, got %d finding(s)", len(f))
	}
	if f[0].Job != "(composite action)" {
		t.Errorf("the finding must read correctly for a file with no jobs, got %q", f[0].Job)
	}
}

func TestRunWalksTheActionTreesToo(t *testing.T) {
	// scanRoots, not Scan(). TestScansCompositeActions proves the PARSER handles a
	// composite action; this proves the guard actually LOOKS in the directory they
	// live in. Deleting the .github/actions roots left that test green — the
	// mutation check is what surfaced it, and only because it asserts the mutation
	// applied before drawing a conclusion.
	root := t.TempDir()
	// A workflow tree must exist too, or the corpus guard fires first and this
	// would pass for the wrong reason.
	wf := filepath.Join(root, ".github", "workflows")
	act := filepath.Join(root, ".github", "actions", "fetch-thing")
	for _, d := range []string{wf, act} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wf, "ok.yml"), []byte("jobs:\n  j:\n    steps:\n      - run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(act, "action.yml"), []byte(`
runs:
  using: composite
  steps:
    - name: Fetch
      run: KUBE_REGION="${{ inputs.region }}"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := Run(root, &out, &errOut); err == nil {
		t.Fatal("an injection inside a composite action must fail the guard — that is where the " +
			"attacker-reachable one was")
	}
	if !strings.Contains(errOut.String(), "action.yml") {
		t.Errorf("the failure must name the action file, got:\n%s", errOut.String())
	}
}

func TestFlagsEnvInterpolationBecauseItIsTheRemedysOwnBackDoor(t *testing.T) {
	// THE HIGHEST-RISK REINTRODUCTION PATH, and it is one this guard CREATED.
	//
	// Every fix this guard prescribes moves the dangerous value into `env:`. So
	// after the remedy the tree is full of env vars holding exactly the
	// attacker-controlled text, and the "obvious" way to use one is
	// `"${{ env.DRIFT_BRANCH }}"` — which is textual substitution into the script
	// again, the identical vulnerability, wearing the shape of the fix. A reviewer
	// scanning for `${{ inputs.` sees compliance.
	//
	// Forbidding it costs nothing: anything reachable as `env.X` is reachable as
	// `$X`, which is the safe form and the one the remedy already writes.
	f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - env:
          DRIFT_BRANCH: ${{ inputs.branch }}
        run: git checkout "${{ env.DRIFT_BRANCH }}"
`))
	if len(f) != 1 {
		t.Fatalf("env.* in a run: script is the remedy laundering the same injection, got %d finding(s)", len(f))
	}
	// The safe form the remedy actually prescribes must stay clean, or the guard
	// forbids its own fix and every site regresses to something worse.
	if g := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - env:
          DRIFT_BRANCH: ${{ inputs.branch }}
        run: git checkout "$DRIFT_BRANCH"
`)); len(g) != 0 {
		t.Fatalf("$DRIFT_BRANCH is the prescribed fix and must pass, got %d finding(s)", len(g))
	}
}

func TestFlagsGithubRefNotJustRefName(t *testing.T) {
	// github.ref is the same attacker-chosen branch name under a refs/heads/
	// prefix. A prefix does not make text safe: `refs/heads/x";id;#` still ends
	// the quoted string. Flagging ref_name while passing ref would leave the
	// cheaper spelling of the identical hole open.
	if !externallySupplied("github.ref") {
		t.Error("github.ref carries the branch name and is attacker-chosen")
	}
	if f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: echo "${{ github.ref }}"
`)); len(f) != 1 {
		t.Fatalf("github.ref in a run: script is an injection, got %d finding(s)", len(f))
	}
}

func TestMatchesAnExpressionThatSpansALine(t *testing.T) {
	// An expression split across lines inside a `run: |` block must still be seen.
	// The regex this began as stopped at the newline, so such a block NEVER
	// MATCHED and the guard reported OK having examined a script it could not
	// parse. Silent under-scanning is the only guard failure mode that matters,
	// because nothing distinguishes it from a clean tree — which is why the
	// delimiting is a scanner now rather than a pattern.
	f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: |
          git checkout "${{
            inputs.branch }}"
`))
	if len(f) != 1 {
		t.Fatalf("a multi-line expression is still an interpolation, got %d finding(s)", len(f))
	}
}

func TestScanRootsCoverBothDeliveredTrees(t *testing.T) {
	// PINNED BECAUSE DELETING THEM LEFT THE SUITE GREEN. A mutation run removing
	// both instance-template roots passed every other test in this file, even
	// though that tree is the one the header calls the one that matters most: its
	// files are DELIVERED, so an injection there is every adopter's, and the real
	// attacker-reachable path this guard was built for lives in exactly that tree.
	// Coverage of the template's own .github/ is not coverage of the shipped one.
	want := []string{
		".github/workflows",
		"instance-template/.github/workflows",
		".github/actions",
		"instance-template/.github/actions",
	}
	have := map[string]bool{}
	for _, r := range scanRoots {
		have[r] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("scanRoots must cover %s — dropping it silently halves what this guard sees", w)
		}
	}
}

func TestFlagsTheWholeGithubContext(t *testing.T) {
	// Two spellings that carry github.event without naming it. Both returned 0
	// findings against the built guard before `case expr == "github"` existed.
	for _, expr := range []string{
		`echo "${{ toJSON(github) }}"`,                     // serialises the entire payload
		`echo "${{ github['event'].pull_request.title }}"`, // the dotted path, written so a dotted matcher misses it
	} {
		f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: `+expr+`
`))
		if len(f) == 0 {
			t.Errorf("%s reaches the event payload and must be flagged", expr)
		}
	}
	// github.repository / github.sha stay clean: server-set, and flagging them
	// would be noise on the most common expressions in the tree.
	if f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: echo "${{ github.repository }} ${{ github.sha }}"
`)); len(f) != 0 {
		t.Fatalf("server-set github.* must stay clean, got %d finding(s)", len(f))
	}
}

func TestMatrixIsCleanUnlessBuiltFromAnInput(t *testing.T) {
	// A literal matrix is the normal case and stays in the deferred tier: the
	// values are written in the file, so pasting one into a script adds nothing an
	// attacker controls.
	if f := Scan("w.yml", parse(t, `
jobs:
  b:
    strategy:
      matrix:
        region: [us-ord, us-sea]
    steps:
      - run: echo "${{ matrix.region }}"
`)); len(f) != 0 {
		t.Fatalf("a literal matrix is not attacker-controlled, got %d finding(s)", len(f))
	}
	// But a matrix BUILT FROM a dispatch input is that input wearing another name,
	// and the deferred tier applied unconditionally passed it straight through.
	if f := Scan("w.yml", parse(t, `
jobs:
  b:
    strategy:
      matrix: ${{ fromJSON(inputs.targets) }}
    steps:
      - run: echo "${{ matrix.region }}"
`)); len(f) != 1 {
		t.Fatalf("matrix.* fed from inputs.* is a dispatch-input injection, got %d finding(s)", len(f))
	}
	// include: is the other shape a matrix takes, and it must taint the same way.
	if f := Scan("w.yml", parse(t, `
jobs:
  b:
    strategy:
      matrix:
        include: ${{ fromJSON(github.event.client_payload.list) }}
    steps:
      - run: echo "${{ matrix.name }}"
`)); len(f) != 1 {
		t.Fatalf("a tainted include: must taint matrix.* too, got %d finding(s)", len(f))
	}
}

func TestADeliveredTreeThatMovedIsAFailureNotAQuietPass(t *testing.T) {
	// The aggregate "did we scan anything" check cannot see this: the template's
	// own .github/ always resolves, so renaming or moving instance-template/'s
	// trees leaves the count comfortably non-zero and the guard prints a confident
	// OK — over exactly the half that reaches every adopter.
	root := t.TempDir()
	wf := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wf, "ok.yml"),
		[]byte("jobs:\n  j:\n    steps:\n      - run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// instance-template/ exists but carries no .github/ — the shape a rename or a
	// move leaves behind.
	if err := os.MkdirAll(filepath.Join(root, "instance-template", "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := Run(root, &out, &errOut); err == nil {
		t.Fatalf("instance-template/ present with no workflow tree must fail, got OK: %s", out.String())
	}

	// The absence is LEGITIMATE in an instance checkout, which has no
	// instance-template/ at all — that must still pass, or the guard cannot run
	// where half the value is. It DOES carry .github/actions: the managed lock
	// delivers six composite actions to every instance, so the fixture has one.
	inst := t.TempDir()
	iwf := filepath.Join(inst, ".github", "workflows")
	iact := filepath.Join(inst, ".github", "actions", "cluster-access")
	for _, d := range []string{iwf, iact} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(iwf, "ok.yml"),
		[]byte("jobs:\n  j:\n    steps:\n      - run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iact, "action.yml"),
		[]byte("runs:\n  using: composite\n  steps:\n    - run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if err := Run(inst, &out, &errOut); err != nil {
		t.Fatalf("an instance checkout has no instance-template/ and must pass: %v", err)
	}
}

func TestAnExpressionInAShellCommentIsStillFlagged(t *testing.T) {
	// NOT A FALSE POSITIVE, and the temptation to "fix" it by stripping `#` lines
	// is how the guard would get a hole with a reasonable-sounding rationale.
	// Substitution happens BEFORE the shell parses, so the value lands inside the
	// comment as text — and a value containing a newline ENDS the comment, leaving
	// whatever follows on its own line to execute. A comment is not a container.
	//
	// This is not hypothetical: the remediation commit put the old dangerous form
	// into a comment as an illustration, and the guard caught it.
	f := Scan("a.yml", parse(t, `
runs:
  using: composite
  steps:
    - run: |
        # the old form was --output "${{ inputs.kubeconfig-path }}"
        echo hi
`))
	if len(f) != 1 {
		t.Fatalf("an expression in a # comment can still break out via a newline, got %d finding(s)", len(f))
	}
}

func TestDoesNotReadIdentifiersInsideStringLiterals(t *testing.T) {
	// A FALSE POSITIVE INTRODUCED BY THE env.* RULE. Text inside a quoted literal
	// is data, not a context reference — but the identifier regex read
	// `hashFiles('**/env.yaml')` as the env context and failed a workflow that
	// interpolates nothing external. The header says false positives are how a
	// guard gets switched off; this one would have arrived with the guard.
	for _, expr := range []string{
		`echo "${{ hashFiles('**/env.yaml') }}"`,
		`echo "${{ hashFiles('inputs.lock') }}"`,
	} {
		if f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: `+expr+`
`)); len(f) != 0 {
			t.Errorf("%s references nothing external, got %d finding(s)", expr, len(f))
		}
	}
	// A real reference OUTSIDE the literal is still caught — stripping literals
	// must not blind the scan.
	if f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: echo "${{ format('{0}', github.event.pull_request.title) }}"
`)); len(f) != 1 {
		t.Fatalf("a reference outside the literal is still an injection, got %d finding(s)", len(f))
	}
}

func TestContextMatchingIsCaseInsensitive(t *testing.T) {
	// GitHub treats context names case-insensitively; and where it does not, the
	// expression is invalid and never runs — so flagging costs nothing either way.
	// A matcher that knows only one spelling is not a matcher.
	for _, ctx := range []string{"Inputs.ref", "GITHUB.EVENT.pull_request.title", "Env.X"} {
		if !externallySupplied(ctx) {
			t.Errorf("%s is the same reference in different case", ctx)
		}
	}
}

func TestABraceInsideALiteralDoesNotEndTheExpression(t *testing.T) {
	// GitHub's own brace-escape for format() is '{{' / '}}', so an expression can
	// legitimately CONTAIN `}}` inside a string literal. A non-greedy regex stopped
	// at that `}}`, never reached inputs.tag, and the guard exited 0 over a live
	// injection — measured, not reasoned.
	f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: echo "${{ format('{{{0}}}', inputs.tag) }}"
`))
	if len(f) != 1 {
		t.Fatalf("a }} inside a literal must not end the expression, got %d finding(s)", len(f))
	}
	// The literal's own contents still must not be read as references.
	if f[0].Expr != "inputs.tag" {
		t.Errorf("the finding must name the real reference, got %q", f[0].Expr)
	}
}

func TestFindExpressionsHandlesAwkwardShapes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{`a ${{ inputs.x }} b ${{ github.event.y }} c`, 2},
		{`${{ format('}}', inputs.x) }}`, 1},
		{`${{ inputs.x`, 0}, // unterminated: reported as itself, not as a block of shell
		// SCANNING RESUMES PAST AN UNDELIMITABLE `${{`. Returning there made
		// everything after it one pseudo-block, so a well-formed expression later in
		// the same script was never delimited as itself. The later expression is now
		// its own block, and the stray opener is reported separately rather than as
		// a block of raw shell.
		{`${{ a[' }} and ${{ inputs.ref }}`, 1},
		{`no expressions here`, 0},
		{`${{}}`, 1},
	} {
		if got, _ := findExpressions(tc.in); len(got) != tc.want {
			t.Errorf("findExpressions(%q) = %d blocks, want %d (%q)", tc.in, len(got), tc.want, got)
		}
	}
}

func TestASiblingOfMatrixDoesNotTaintALiteralMatrix(t *testing.T) {
	// Flattening the whole strategy node let `max-parallel` decide whether the
	// MATRIX was attacker-built. The matrix here is literal, so matrix.region is
	// safe, and a finding would name the wrong problem entirely.
	if f := Scan("w.yml", parse(t, `
jobs:
  b:
    strategy:
      max-parallel: ${{ inputs.parallelism }}
      matrix:
        region: [us-ord, us-sea]
    steps:
      - run: echo "${{ matrix.region }}"
`)); len(f) != 0 {
		t.Fatalf("a literal matrix is safe whatever its siblings say, got %d finding(s): %+v", len(f), f)
	}
	// And a genuinely tainted matrix beside the same sibling is still caught.
	if f := Scan("w.yml", parse(t, `
jobs:
  b:
    strategy:
      max-parallel: ${{ inputs.parallelism }}
      matrix: ${{ fromJSON(inputs.targets) }}
    steps:
      - run: echo "${{ matrix.region }}"
`)); len(f) != 1 {
		t.Fatalf("a tainted matrix must still be caught, got %d finding(s)", len(f))
	}
}

func TestAStrayOpenExpressionDoesNotSwallowTheRest(t *testing.T) {
	// MEASURED AT ZERO FINDINGS before this was fixed. An undelimitable `${{`
	// made the scanner return, so everything after it became one pseudo-block —
	// and the literal-stripping pass then deleted the real expressions out of that
	// raw shell text. The genuine `${{ inputs.ref }}` below simply vanished.
	f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: |
          echo "${{ vars.A[' }}"
          llz upgrade --ref "${{ inputs.ref }}"
`))
	found := false
	for _, x := range f {
		if x.Expr == "inputs.ref" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the injection after a stray ${{ must still be found, got %+v", f)
	}
}

func TestQuoteTrackingIsSingleQuoteOnly(t *testing.T) {
	// GitHub expression syntax has no double-quoted strings. Treating `"` as a
	// quote made the scanner mis-track state against ordinary shell text, which is
	// what let the case above swallow real expressions.
	f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: echo "${{ inputs.a }}" && echo "${{ inputs.b }}"
`))
	if len(f) != 2 {
		t.Fatalf("both expressions must be found across double quotes, got %d: %+v", len(f), f)
	}
}

func TestMatrixRefMatchingIsCaseInsensitive(t *testing.T) {
	// externallySupplied folds case; this half did not, so a tainted matrix read
	// as ${{ Matrix.region }} returned zero findings — the failure the sibling
	// function's own comment forbids.
	for _, ref := range []string{"Matrix.region", "MATRIX.region", "matrix"} {
		if !isMatrixRef(ref) {
			t.Errorf("%s is a matrix reference", ref)
		}
	}
	if f := Scan("w.yml", parse(t, `
jobs:
  b:
    strategy:
      matrix: ${{ fromJSON(inputs.targets) }}
    steps:
      - run: echo "${{ Matrix.region }}"
`)); len(f) != 1 {
		t.Fatalf("a tainted matrix read in any case is an injection, got %d finding(s)", len(f))
	}
}

func TestGithubScriptBodyIsScannedToo(t *testing.T) {
	// actions/github-script's `with: script:` is JavaScript, and the same class:
	// the expression is substituted into source before the interpreter parses it,
	// so the value closes the string and runs.
	f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - uses: actions/github-script@v7
        with:
          script: |
            core.info("${{ github.event.issue.title }}")
`))
	if len(f) != 1 {
		t.Fatalf("a github-script body is interpreted source, got %d finding(s)", len(f))
	}
}

func TestAnEmptyRootSaysEmptyNotMoved(t *testing.T) {
	// A present-but-empty root is not a moved tree, and reporting the wrong cause
	// sends the reader looking for a rename that never happened.
	root := t.TempDir()
	wf := filepath.Join(root, ".github", "workflows")
	act := filepath.Join(root, ".github", "actions")
	for _, d := range []string{wf, act} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wf, "ok.yml"),
		[]byte("jobs:\n  j:\n    steps:\n      - run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := Run(root, &out, &errOut)
	if err == nil {
		t.Fatal("an empty scan root must fail")
	}
	if !strings.Contains(err.Error(), "holds no workflows") {
		t.Errorf("the cause must be 'empty', not 'moved': %v", err)
	}
}

func TestRenamingTheWholeDeliveredDotGithubIsCaught(t *testing.T) {
	// path.Dir("instance-template/.github/workflows") is "instance-template/.github"
	// — which is ITSELF part of what a rename moves. So `mv instance-template/.github
	// instance-template/dot-github` made the discriminator fail, the roots looked
	// "not applicable", and the guard printed OK over the first-party files with all
	// 22 delivered ones unread. The question is whether the TREE is present, and only
	// instance-template/ answers that.
	root := t.TempDir()
	wf := filepath.Join(root, ".github", "workflows")
	act := filepath.Join(root, ".github", "actions", "a")
	moved := filepath.Join(root, "instance-template", "dot-github", "workflows")
	for _, d := range []string{wf, act, moved} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte("jobs:\n  j:\n    steps:\n      - run: echo hi\n")
	for _, f := range []string{filepath.Join(wf, "ok.yml"), filepath.Join(moved, "ok.yml")} {
		if err := os.WriteFile(f, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(act, "action.yml"),
		[]byte("runs:\n  using: composite\n  steps:\n    - run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := Run(root, &out, &errOut)
	if err == nil {
		t.Fatalf("a renamed instance-template/.github must fail, got OK: %s", out.String())
	}
	if !strings.Contains(err.Error(), "moved") {
		t.Errorf("the cause must be 'moved', got: %v", err)
	}
}

func TestAnInjectionIsReportedOnce(t *testing.T) {
	// The undelimitable-`${{` path reports the remainder as a pseudo-block AND
	// resumes scanning inside it, so a later expression was found twice — one
	// injection printed as two identical lines and a count of 2. A guard that
	// double-counts is one whose numbers stop being read.
	f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: |
          echo "${{ vars.A[' }}"
          llz upgrade --ref "${{ inputs.ref }}"
`))
	// Two DISTINCT findings, not one thing said twice: the malformed opener and
	// the injection after it. Each appears exactly once.
	got := map[string]int{}
	for _, x := range f {
		got[x.Expr]++
	}
	if got["inputs.ref"] != 1 {
		t.Errorf("the injection must be reported exactly once, got %d (%+v)", got["inputs.ref"], f)
	}
	if got["(unterminated ${{ )"] != 1 {
		t.Errorf("the stray opener must be named, got %+v", f)
	}
	if len(f) != 2 {
		t.Errorf("no other findings expected, got %+v", f)
	}
}

func TestPlainShellAfterAStrayOpenerIsNotAnInjection(t *testing.T) {
	// THE FALSE-POSITIVE HALF. Emitting the remainder as a pseudo-block scanned
	// raw shell as expression text, so ordinary commands were reported as
	// injections — `cat env.yaml` became `env.yaml`, `-f inputs.values.yaml` became
	// `inputs.values.yaml`, each with a "move it to env:" remedy that makes no
	// sense. False positives on ordinary commands are how a guard gets switched
	// off, which the header already says.
	f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: |
          echo "${{ vars.A[' }}"
          cat env.yaml
          helm upgrade -f inputs.values.yaml
`))
	for _, x := range f {
		if x.Expr != "(unterminated ${{ )" {
			t.Errorf("plain shell must not be read as an expression, got %q", x.Expr)
		}
	}
	if len(f) != 1 {
		t.Fatalf("only the stray opener is a finding here, got %+v", f)
	}
}

func TestWorkflowRefCarriesTheBranchNameToo(t *testing.T) {
	// github.workflow_ref is owner/repo/.github/workflows/x.yml@refs/heads/<branch>
	// — the same attacker-chosen text wrapped in more text, which does not make it
	// safe. Flagged alongside github.ref rather than left as a silent omission.
	if !externallySupplied("github.workflow_ref") {
		t.Error("github.workflow_ref embeds the branch name")
	}
	if f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: echo "${{ github.workflow_ref }}"
`)); len(f) != 1 {
		t.Fatalf("github.workflow_ref in a run: script is an injection, got %d finding(s)", len(f))
	}
}

func TestAWrongRootSaysWrongRoot(t *testing.T) {
	// This check was UNREACHABLE: it ran after the per-root loop, and
	// .github/workflows always returned from there first, so pointing the guard at
	// an empty directory reported "the tree moved" — sending the reader to look
	// for a rename when the actual mistake was --root.
	var out, errOut bytes.Buffer
	err := Run(t.TempDir(), &out, &errOut)
	if err == nil {
		t.Fatal("an empty tree must fail")
	}
	if !strings.Contains(err.Error(), "refusing to pass vacuously") {
		t.Errorf("the cause must be the wrong root, got: %v", err)
	}
}

func TestFindingOrderIsTotalAndStable(t *testing.T) {
	// Comparing only (File, Job) ties on every finding within a job, and
	// sort.Slice is unstable, so findings within a job printed in arbitrary order —
	// undiffable output, and a golden test impossible. Asserting the EXACT
	// sequence, because "the same twice in a row" passes even with no order at all.
	src := `
jobs:
  b:
    steps:
      - name: s1
        run: echo "${{ inputs.b }} ${{ inputs.a }}"
      - name: s0
        run: echo "${{ inputs.z }}"
`
	root := t.TempDir()
	wf := filepath.Join(root, ".github", "workflows")
	act := filepath.Join(root, ".github", "actions", "a")
	for _, d := range []string{wf, act} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wf, "a.yml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(act, "action.yml"),
		[]byte("runs:\n  using: composite\n  steps:\n    - run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := Run(root, &out, &errOut); err == nil {
		t.Fatal("expected findings")
	}
	// Step ascending, then Expr ascending: s0 before s1, and inputs.a before
	// inputs.b inside s1. Without the Step/Expr tiebreakers this comes out in
	// whatever order the scan produced.
	// s0 sorts before s1, and inputs.z sorts AFTER both of s1's references — so
	// this sequence is only produced when Step is compared before Expr. The first
	// fixture used github.event.x, whose Expr order happened to match its Step
	// order, and passed with the Step tiebreaker removed.
	want := []string{"inputs.z", "inputs.a", "inputs.b"}
	var got []string
	for _, line := range strings.Split(errOut.String(), "\n") {
		for _, w := range want {
			if strings.Contains(line, "${{ "+w+" ") || strings.Contains(line, w+" }}") {
				got = append(got, w)
			}
		}
	}
	// Each finding is printed twice (annotation + summary); collapse consecutive
	// repeats of the whole sequence by taking the first len(want).
	if len(got) < len(want) {
		t.Fatalf("expected %d findings in the output, got %v\n%s", len(want), got, errOut.String())
	}
	got = got[:len(want)]
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("findings must print in (Step, Expr) order: got %v, want %v\n%s", got, want, errOut.String())
		}
	}
}

func TestAStrayOpenerDoesNotStealALaterExpressionsClose(t *testing.T) {
	// THE SHAPE THE PREVIOUS FIXTURE MISSED: it had no later `}}`, so the stray
	// opener ran off the end and the unterminated flag was set the easy way. Here
	// the opener finds the closing `}}` of a GENUINE later expression, so the
	// intervening shell became one block and `cat env.yaml` was reported as an
	// injection with a "move it to env:" remedy.
	//
	// Expressions do not nest, so a second `${{` while still hunting the first
	// one's `}}` is proof the first never closed. That is a rule, not a heuristic.
	f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: |
          echo "${{ vars.A["
          cat env.yaml
          llz upgrade --ref "${{ inputs.ref }}"
`))
	got := map[string]int{}
	for _, x := range f {
		got[x.Expr]++
	}
	if got["env.yaml"] != 0 {
		t.Errorf("plain shell must not be read as an expression, got %+v", f)
	}
	if got["inputs.ref"] != 1 {
		t.Errorf("the real injection must be found exactly once, got %+v", f)
	}
	if got["(unterminated ${{ )"] != 1 {
		t.Errorf("the stray opener must be named, got %+v", f)
	}
}

func TestTheGuardRunsInAJobForkPullRequestsReach(t *testing.T) {
	// THE PLACEMENT IS THE SECURITY PROPERTY, and until now it was asserted only
	// in a comment. This guard reached CI solely through `llz ci gates` in the
	// Kubernetes job, which carries a `head.repo.full_name == github.repository`
	// condition — so it ran on every population EXCEPT fork pull requests, which
	// is how an attacker-chosen branch name or PR title reaches a workflow at all.
	// Moving the step back, or adding that condition to the job it now lives in,
	// would leave the whole tree green and fork PRs unscanned again.
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", ".github", "workflows", "lint.yml"))
	if err != nil {
		t.Fatalf("reading lint.yml: %v", err)
	}
	// Split into jobs by top-level (2-space) keys under jobs:.
	//
	// COMMENT LINES ARE DROPPED FIRST, and that is not tidiness. The check is a
	// substring match over the job body, so before this the fork condition could
	// not be DESCRIBED in a comment inside the job it applies to — documenting the
	// property failed the test that holds it, which is a gate that punishes the
	// thing it exists to encourage. It read prose as configuration.
	type job struct{ name, body string }
	var jobs []job
	var cur *job
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if len(line) > 2 && line[0] == ' ' && line[1] == ' ' && line[2] != ' ' && strings.HasSuffix(strings.TrimSpace(line), ":") {
			jobs = append(jobs, job{name: strings.TrimSpace(line)})
			cur = &jobs[len(jobs)-1]
			continue
		}
		if cur != nil {
			cur.body += line + "\n"
		}
	}
	if len(jobs) == 0 {
		t.Fatal("parsed no jobs out of lint.yml — the check would be vacuous")
	}
	// EVERY SPELLING OF THE SAME SKIP, because one of them was all this checked and
	// the condition has three equivalent forms. Reversing the operands or asking
	// `.fork` instead is the same exclusion written differently, and would have
	// re-opened the hole under a passing test.
	forkGates := []string{
		"head.repo.full_name == github.repository",
		"github.repository == github.event.pull_request.head.repo.full_name",
		"head.repo.fork",
	}
	found := false
	for _, j := range jobs {
		if !strings.Contains(j.body, "ci workflow-injection") {
			continue
		}
		found = true
		for _, g := range forkGates {
			if strings.Contains(j.body, g) {
				t.Errorf("job %s runs workflow-injection but is skipped for fork PRs (%s) — "+
					"the population the guard exists for", j.name, g)
			}
		}
	}
	if !found {
		t.Error("no lint.yml job runs `llz ci workflow-injection`")
	}
}

func TestABraceTypoIsNotReportedAsAnInjection(t *testing.T) {
	// Two defects, two remedies. A `${{` with no `}}` is a syntax error GitHub
	// rejects; printed under the injection headline it inherited "move it to env:",
	// which cannot fix a brace typo, and was counted as an interpolation of
	// externally-supplied input, which it is not.
	root := t.TempDir()
	wf := filepath.Join(root, ".github", "workflows")
	act := filepath.Join(root, ".github", "actions", "a")
	for _, d := range []string{wf, act} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wf, "w.yml"),
		[]byte("jobs:\n  b:\n    steps:\n      - run: echo \"${{ inputs.x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(act, "action.yml"),
		[]byte("runs:\n  using: composite\n  steps:\n    - run: echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := Run(root, &out, &errOut)
	if err == nil {
		t.Fatal("an unterminated expression must fail the guard")
	}
	if strings.Contains(err.Error(), "interpolation(s) of externally-supplied") {
		t.Errorf("a brace typo is not an interpolation of external input: %v", err)
	}
	if !strings.Contains(err.Error(), "unterminated") {
		t.Errorf("the error must name the real defect, got: %v", err)
	}
	if strings.Contains(errOut.String(), "REF: ${{ inputs.ref }}") {
		t.Error("the env: remedy cannot fix a brace typo and must not be printed for one")
	}
	if !strings.Contains(errOut.String(), "never closed") {
		t.Errorf("the output must name the real defect:\n%s", errOut.String())
	}
}

func TestTheSameReferenceTwiceInAStepIsOneFinding(t *testing.T) {
	// The dedup's comment claims it is "not vestigial"; nothing proved it. One
	// reference used twice in a step is one defect with one fix, and printing it
	// twice inflates the count a reader uses to judge the file. It is also what
	// makes (File, Job, Step, Expr) unique, which is what makes the sort total.
	f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - name: s
        run: echo "${{ inputs.ref }}" && echo "${{ inputs.ref }}"
`))
	if len(f) != 1 {
		t.Fatalf("one reference twice in a step is one finding, got %d: %+v", len(f), f)
	}
}

func TestTwoUnnamedStepsWithTheSameExpressionAreTwoFindings(t *testing.T) {
	// The dedup keyed on the PRINTED step name, which defaults to a constant, so
	// two distinct steps collapsed into one finding. Under-reporting rather than a
	// false pass — the gate still fails — but a reader counting findings to judge
	// how bad a file is gets the wrong number, and two steps need two fixes.
	f := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: echo "${{ inputs.ref }}"
      - run: echo "${{ inputs.ref }}"
`))
	if len(f) != 2 {
		t.Fatalf("two steps needing two fixes are two findings, got %d: %+v", len(f), f)
	}
	// And the same expression twice within ONE step is still one finding.
	if g := Scan("w.yml", parse(t, `
jobs:
  b:
    steps:
      - run: echo "${{ inputs.ref }}" && echo "${{ inputs.ref }}"
`)); len(g) != 1 {
		t.Fatalf("one step is one fix, got %d: %+v", len(g), g)
	}
}
