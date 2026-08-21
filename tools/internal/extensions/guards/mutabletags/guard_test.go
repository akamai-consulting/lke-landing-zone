package mutabletags

// guard_test.go — the decision, one arm at a time.
//
// THE LIVE FILE IS ONE OF THE FIXTURES, and deliberately: the arms below prove
// what the judgement says about hand-written shapes, and TestLiveWorkflowPasses
// proves it says the right thing about the workflow that actually publishes the
// images. A guard held only against fixtures is a guard that agrees with the test
// author.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// good is the shape the workflow ships: sha- outside the gate, mutable inside,
// the gate computed from github.ref.
const good = `
env:
  SHA: ${{ inputs.sha || github.sha }}
  PUBLISH_MUTABLE: ${{ (github.ref == 'refs/heads/main' || github.ref == 'refs/heads/master') && (inputs.sha == '' || inputs.sha == github.sha) }}
jobs:
  build:
    steps:
      - name: Build and push
        run: |
          set -euo pipefail
          NAMES=("${IMAGE}"); [ -z "${ALIAS}" ] || NAMES+=("${ALIAS}"); TAGS=()
          for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); done
          if [ "${PUBLISH_MUTABLE}" = "true" ]; then
            for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:latest"); [ -z "${VERSION}" ] || TAGS+=(--tag "${REPO}/${NAME}:${VERSION}"); done
          fi
          docker buildx build --push "${TAGS[@]}" .
`

// rep is strings.Replace with the mutation ASSERTED. A fixture edit that silently
// matched nothing leaves the good shape behind, and every "this must be rejected"
// case would then fail loudly — but the three "this must still pass" cases would
// pass vacuously, proving nothing about the shape they were written for.
func rep(t *testing.T, body, old, replacement string) string {
	t.Helper()
	out := strings.Replace(body, old, replacement, 1)
	if out == body {
		t.Fatalf("fixture edit matched nothing: %q", old)
	}
	return out
}

func judgeBody(t *testing.T, body string) []problem {
	t.Helper()
	return judge(body, scanFile(body))
}

func msgs(ps []problem) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(p.msg)
		b.WriteString("\n")
	}
	return b.String()
}

func TestGoodShapePasses(t *testing.T) {
	if ps := judgeBody(t, good); len(ps) != 0 {
		t.Fatalf("the shipped shape must pass, got:\n%s", msgs(ps))
	}
}

func TestUngatedMutableTagIsRejected(t *testing.T) {
	// The defect this gate exists for: `:latest` published from whatever ref the
	// build ran on — which is what #451 describes and what the tree did.
	body := rep(t, good,
		`          if [ "${PUBLISH_MUTABLE}" = "true" ]; then`+"\n",
		"")
	body = rep(t, body, "          fi\n", "")
	ps := judgeBody(t, body)
	if !strings.Contains(msgs(ps), "MUTABLE tag published from any ref") {
		t.Fatalf("an ungated `:latest` must be rejected, got:\n%s", msgs(ps))
	}
	// It names the tag AND the line, because a workflow has several `--tag` lines
	// and "one of them is wrong" sends the reader back to read all of them.
	found := false
	for _, p := range ps {
		if strings.Contains(p.msg, ":latest") && p.line > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the finding must name the tag and its line, got: %+v", ps)
	}
}

func TestVersionTagIsMutableToo(t *testing.T) {
	// A version tag is republished unchanged on every main build, so a branch
	// moves it exactly as it moves `:latest`. Anything not `sha-` is mutable, and
	// this pins that the classification is by prefix rather than by an allowlist
	// of names somebody has to remember to extend.
	body := rep(t, good,
		`for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); done`,
		`for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); TAGS+=(--tag "${REPO}/${NAME}:${VERSION}"); done`)
	if !strings.Contains(msgs(judgeBody(t, body)), "MUTABLE tag published from any ref") {
		t.Fatalf("an ungated version tag must be rejected, got:\n%s", msgs(judgeBody(t, body)))
	}
}

func TestGateThatIgnoresTheRefIsRejected(t *testing.T) {
	// `PUBLISH_MUTABLE: true` satisfies the "inside the gate" arm perfectly and
	// publishes from everywhere, which is why the expression is checked too.
	body := rep(t, good,
		`  PUBLISH_MUTABLE: ${{ (github.ref == 'refs/heads/main' || github.ref == 'refs/heads/master') && (inputs.sha == '' || inputs.sha == github.sha) }}`,
		`  PUBLISH_MUTABLE: true`)
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "sanctioned gate expression") {
		t.Fatalf("a gate that ignores the ref must be rejected, got:\n%s", got)
	}
}

func TestMissingGateEntryIsRejected(t *testing.T) {
	body := rep(t, good,
		`  PUBLISH_MUTABLE: ${{ (github.ref == 'refs/heads/main' || github.ref == 'refs/heads/master') && (inputs.sha == '' || inputs.sha == github.sha) }}`+"\n",
		"")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "never sets") {
		t.Fatalf("an unset gate variable must be rejected, got:\n%s", got)
	}
}

func TestNoMutableTagAtAllIsRejected(t *testing.T) {
	// Deleting the `:latest` publish would satisfy every "is it gated" arm
	// vacuously — and break the tag version-pins requires this repo's container
	// jobs to float on. The expected set must not come from the thing under test.
	body := rep(t, good,
		`            for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:latest"); [ -z "${VERSION}" ] || TAGS+=(--tag "${REPO}/${NAME}:${VERSION}"); done`+"\n",
		"")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "no mutable tag is published at all") {
		t.Fatalf("dropping the mutable publish must be rejected, got:\n%s", got)
	}
}

func TestShaTagMovedInsideTheGateIsRejected(t *testing.T) {
	// The other side of the same trap: gate the sha- tag and a branch build
	// publishes nothing, so pin-instance-images waits out its budget on an image
	// that was never going to appear.
	body := rep(t, good,
		`          for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); done`+"\n",
		"")
	body = rep(t, body,
		`            for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:latest");`,
		`            for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); TAGS+=(--tag "${REPO}/${NAME}:latest");`)
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "outside the `PUBLISH_MUTABLE` gate") {
		t.Fatalf("a gated sha- tag must be rejected, got:\n%s", got)
	}
}

func TestEmptyCorpusFailsClosed(t *testing.T) {
	for _, body := range []string{"", "name: nothing\non:\n  push:\n"} {
		if got := msgs(judgeBody(t, body)); !strings.Contains(got, "no `--tag` argument found") {
			t.Fatalf("a file with no publish must fail closed, got:\n%s", got)
		}
	}
}

func TestShortTagFlagIsRefused(t *testing.T) {
	// `-t` would publish past a guard that reads only `--tag`, so it is refused
	// rather than parsed.
	body := rep(t, good,
		`docker buildx build --push "${TAGS[@]}" .`,
		`docker buildx build --push -t "${REPO}/${IMAGE}:latest" .`)
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "short `-t` tag flag is refused") {
		t.Fatalf("the short flag must be refused, got:\n%s", got)
	}
}

func TestProseIsNotCode(t *testing.T) {
	// Comments explain this rule at length in the workflow — including the words
	// `if`, `-t` and `:latest`. A guard that reads its own documentation as a
	// violation is one that gets deleted.
	body := rep(t, good, "          set -euo pipefail\n",
		"          # if we published :latest here, or used -t, this would be the bug\n"+
			"          set -euo pipefail   # if -t appeared above it is prose\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("comments must not be read as code, got:\n%s", msgs(ps))
	}
}

func TestJobLevelIfIsNotAShellIf(t *testing.T) {
	// Actions' own `if:` is spelled with a colon. Counting it as an open shell
	// block would leave every later tag looking enclosed by whatever came before.
	body := rep(t, good,
		"    steps:\n",
		"    if: ${{ github.event_name == 'push' && env.PUBLISH_MUTABLE }}\n    steps:\n")
	body = rep(t, body,
		`          if [ "${PUBLISH_MUTABLE}" = "true" ]; then`+"\n", "")
	body = rep(t, body, "          fi\n", "")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "MUTABLE tag published from any ref") {
		t.Fatalf("a job-level `if:` must not gate a shell tag, got:\n%s", got)
	}
}

func TestOneLineIfCountsAsGated(t *testing.T) {
	body := rep(t, good,
		`          if [ "${PUBLISH_MUTABLE}" = "true" ]; then`+"\n"+
			`            for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:latest"); [ -z "${VERSION}" ] || TAGS+=(--tag "${REPO}/${NAME}:${VERSION}"); done`+"\n"+
			"          fi\n",
		`          if [ "${PUBLISH_MUTABLE}" = "true" ]; then TAGS+=(--tag "${REPO}/${IMAGE}:latest"); fi`+"\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("a one-line if must count as gated, got:\n%s", msgs(ps))
	}
}

func TestNestedIfDoesNotEscapeTheGate(t *testing.T) {
	// An inner `if` closing must not pop the gate — the classic off-by-one in a
	// depth scanner, and the one that would silently un-gate everything after it.
	body := rep(t, good,
		`            for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:latest"); [ -z "${VERSION}" ] || TAGS+=(--tag "${REPO}/${NAME}:${VERSION}"); done`,
		"            if [ -n \"${VERSION}\" ]; then echo versioned; fi\n"+
			`            TAGS+=(--tag "${REPO}/${IMAGE}:latest")`)
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("a nested if must not release the gate, got:\n%s", msgs(ps))
	}
}

func TestUnparseableTagReferenceIsAFinding(t *testing.T) {
	// "Could not tell" is not "immutable".
	body := rep(t, good,
		`--tag "${REPO}/${NAME}:latest"`, `--tag "${REPO}/${NAME}"`)
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "cannot read a tag out of") {
		t.Fatalf("a tagless reference must be reported, got:\n%s", got)
	}
}

func TestRunFailsOnAMissingPublisher(t *testing.T) {
	// A rename must not turn this gate into a green check over nothing.
	var out, errOut bytes.Buffer
	err := Run(t.TempDir(), &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), publisherFile) {
		t.Fatalf("a missing publisher must fail and name the file, got err=%v", err)
	}
}

// TestLiveWorkflowPasses runs the judgement against the real build-images.yml.
// The fixtures above prove what the rule says; this proves the rule is true of
// the file it was written for — and that a future edit to that file has to keep
// it true.
func TestLiveWorkflowPasses(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", publisherFile))
	if err != nil {
		t.Fatalf("read the live publisher: %v", err)
	}
	sc := scanFile(string(body))
	if sc.parseErr != nil {
		t.Fatalf("the live publisher must parse: %v", sc.parseErr)
	}
	if len(sc.sites) == 0 {
		t.Fatal("the live workflow published no --tag — the fixture-free half of this test would be vacuous")
	}
	// The live file's own scripts must also come out BALANCED. That is what proves
	// the scanner is still reading them as shell rather than losing the block
	// structure in prose — the defect that made an earlier cut of this guard blind.
	if len(sc.unbalanced) != 0 {
		t.Fatalf("the live publisher's scripts did not close their if blocks: %v", sc.unbalanced)
	}
	if ps := judge(string(body), sc); len(ps) != 0 {
		t.Fatalf("the live %s must satisfy this gate, got:\n%s", publisherFile, msgs(ps))
	}
}

func TestRunReportsTheLiveWorkflowClean(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run(filepath.Join("..", "..", "..", "..", ".."), &out, &errOut); err != nil {
		t.Fatalf("Run over the repo root: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "mutable-tag-guard: OK") {
		t.Fatalf("want an OK verdict, got %q", out.String())
	}
}

// ── The bypasses a review found in the first cut ─────────────────────────────
//
// Every one of these made the guard report OK over the #451 defect itself, which
// is the worst failure a gate has: it launders the bug into a green check. They
// are kept as named cases rather than folded into the arms above, because each
// one is a way the scanner can go blind rather than a way the workflow can be
// wrong.

func TestQuotedIfInProseDoesNotOpenABlock(t *testing.T) {
	// A shell `if` inside a STRING is not a block. The scanner used to push one and
	// never pop it, so every later tag counted as enclosed — and if that string also
	// mentioned PUBLISH_MUTABLE, it counted as GATED. The live workflow already had
	// two such lines (an input `description:` and an `::error::` echo); only the
	// absence of the gate's name on them kept the guard sighted.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -euo pipefail\n"+
			`          echo "PUBLISH_MUTABLE=${PUBLISH_MUTABLE} — :latest is published only if this is true"`+"\n")
	body = rep(t, body, `          if [ "${PUBLISH_MUTABLE}" = "true" ]; then`+"\n", "")
	body = rep(t, body, "          fi\n", "")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "MUTABLE tag published from any ref") {
		t.Fatalf("a quoted `if` must not gate anything, got:\n%s", got)
	}
}

func TestYAMLProseIsNotShell(t *testing.T) {
	// The same defect one level out: only `run:` scripts are shell. A workflow
	// input's description is prose, and the live file's says "…matches even if the
	// branch HEAD has since moved".
	body := rep(t, good, "jobs:\n",
		"on:\n  workflow_dispatch:\n    inputs:\n      sha:\n"+
			"        description: rebuild if PUBLISH_MUTABLE is set and the branch HEAD has moved\n"+
			"jobs:\n")
	body = rep(t, body, `          if [ "${PUBLISH_MUTABLE}" = "true" ]; then`+"\n", "")
	body = rep(t, body, "          fi\n", "")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "MUTABLE tag published from any ref") {
		t.Fatalf("YAML prose must not gate a shell tag, got:\n%s", got)
	}
}

func TestInvertedGateIsRejected(t *testing.T) {
	// #451 EXACTLY INVERTED: publish the mutable tags from every ref EXCEPT the
	// default branch. Lexically it is inside the gate, so "is it enclosed" passes
	// it. The condition has to be read, not just located.
	body := rep(t, good, `if [ "${PUBLISH_MUTABLE}" = "true" ]; then`, `if [ "${PUBLISH_MUTABLE}" != "true" ]; then`)
	got := msgs(judgeBody(t, body))
	// Reported as the non-canonical spelling it is — the message names `!=` and
	// says what it does, which is the sentence this author needs. What must not
	// happen is silence.
	if !strings.Contains(got, "canonical") || !strings.Contains(got, "!=") {
		t.Fatalf("an inverted gate must be rejected and named, got:\n%s", got)
	}
}

func TestElseArmIsNotGated(t *testing.T) {
	// The else arm of the gate runs when the gate is FALSE — a mutable tag there is
	// published from exactly the refs the gate exists to exclude.
	body := rep(t, good, "          fi\n",
		"          else\n"+
			`            TAGS+=(--tag "${REPO}/${IMAGE}:latest")`+"\n"+
			"          fi\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "MUTABLE tag published from any ref") {
		t.Fatalf("the else arm must not count as gated, got:\n%s", got)
	}
}

func TestEqualsFormOfTheTagFlagIsSeen(t *testing.T) {
	// `--tag=<ref>` is the same publish spelled with an `=`. Reading only the
	// space-separated form is the bypass class this guard refuses `-t` for.
	body := rep(t, good, `for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); done`,
		`for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}" --tag="${REPO}/${NAME}:nightly"); done`)
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "MUTABLE tag published from any ref") {
		t.Fatalf("`--tag=` must be read like `--tag `, got:\n%s", got)
	}
}

func TestUnrelatedShortFlagIsNotADockerTag(t *testing.T) {
	// `-t` is refused so a publish cannot hide behind it — but `mktemp -t`,
	// `sort -t` and friends are not publishes, and failing the gate on them with a
	// docker-tag diagnosis is a guard that gets turned off.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -euo pipefail\n          TMP=$(mktemp -d -t llz)\n          sort -t , -k1 <<<\"a,b\" >/dev/null\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("an unrelated -t must not be read as a docker tag, got:\n%s", msgs(ps))
	}
}

func TestUnbalancedScriptFailsClosed(t *testing.T) {
	// If the scanner loses track of the block structure it can no longer say what
	// is gated. "Could not tell" is a failure, not a pass.
	body := rep(t, good, "          fi\n", "")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "did not close") {
		t.Fatalf("an unbalanced script must fail closed, got:\n%s", got)
	}
}

func TestNonCanonicalGateSpellingSaysSo(t *testing.T) {
	// A reader who wrote a REAL gate in another form must be told that, not told to
	// "move it inside the gate" it is already inside.
	body := rep(t, good,
		`          if [ "${PUBLISH_MUTABLE}" = "true" ]; then`+"\n"+
			`            for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:latest"); [ -z "${VERSION}" ] || TAGS+=(--tag "${REPO}/${NAME}:${VERSION}"); done`+"\n"+
			"          fi\n",
		`          [ "${PUBLISH_MUTABLE}" != "true" ] || TAGS+=(--tag "${REPO}/${IMAGE}:latest")`+"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "canonical") {
		t.Fatalf("a non-canonical gate must be named as such, got:\n%s", got)
	}
}

func TestMalformedYAMLFailsClosed(t *testing.T) {
	if got := msgs(judgeBody(t, "jobs:\n  a: [unterminated\n")); !strings.Contains(got, "could not be parsed") {
		t.Fatalf("unparseable YAML must fail closed, got:\n%s", got)
	}
}

// ── Round two: the same three defects one level finer ────────────────────────

func TestSameLineElseIsNotGated(t *testing.T) {
	// The multi-line else was fixed by clearing the stack entry; the ONE-LINE else
	// was not, because the line's tags were judged before its own tokens were read.
	// Same publish, same inversion, spelled on one line.
	body := rep(t, good,
		`          if [ "${PUBLISH_MUTABLE}" = "true" ]; then`+"\n"+
			`            for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:latest"); [ -z "${VERSION}" ] || TAGS+=(--tag "${REPO}/${NAME}:${VERSION}"); done`+"\n"+
			"          fi\n",
		`          if [ "${PUBLISH_MUTABLE}" = "true" ]; then echo gated; else TAGS+=(--tag "${REPO}/${IMAGE}:latest"); fi`+"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "MUTABLE tag published from any ref") {
		t.Fatalf("a one-line else arm must not count as gated, got:\n%s", got)
	}
}

func TestTagAfterASameLineFiIsNotGated(t *testing.T) {
	// Once `fi` has run, the gate is over — a tag after it on the same line is
	// published unconditionally.
	body := rep(t, good,
		`            for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:latest"); [ -z "${VERSION}" ] || TAGS+=(--tag "${REPO}/${NAME}:${VERSION}"); done`+"\n"+
			"          fi\n",
		`            echo gated`+"\n"+
			`          fi; TAGS+=(--tag "${REPO}/${IMAGE}:latest")`+"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "MUTABLE tag published from any ref") {
		t.Fatalf("a tag after `fi` must not count as gated, got:\n%s", got)
	}
}

func TestAStepLevelOverrideOfTheGateIsRejected(t *testing.T) {
	// GitHub resolves step env OVER workflow env, so a step-level
	// `PUBLISH_MUTABLE: 'true'` defeats the gate at runtime while the workflow-level
	// expression above it still reads perfectly. Reading only the first entry is
	// reading the one that loses.
	body := rep(t, good, "      - name: Build and push\n",
		"      - name: Build and push\n        env:\n          PUBLISH_MUTABLE: 'true'\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "sanctioned gate expression") {
		t.Fatalf("a step-level override must be rejected, got:\n%s", got)
	}
}

func TestDockerRunTtyIsNotAPublish(t *testing.T) {
	// `-t` is refused because a PUBLISH could hide behind it. `docker run -t` is not
	// a publish — and the live workflow runs the built image to check its baked
	// version stamp, one edit away from this shape.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		"          docker buildx build --push \"${TAGS[@]}\" .\n"+
			`          docker run --rm -t "${REPO}/${IMAGE}:sha-${SHA}" version`+"\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("`docker run -t` must not be read as a docker BUILD tag, got:\n%s", msgs(ps))
	}
}

// ── Round three: a string is not a publish, and a build is not one line ──────

func TestATagInsideAMessageIsRefusedNotClassified(t *testing.T) {
	// THIS TEST USED TO ASSERT THE OPPOSITE, and the reversal is the point rather
	// than an accident. It required an `::error::` message naming `--tag` to pass
	// clean, which is the humane answer and the one that cannot be implemented: the
	// same shape with an argument after the flag is a publish, and four successive
	// rules for telling the two apart each let one through or reddened the other.
	// The message is now refused, which costs a rewording and ends the class.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		"          docker buildx build --push \"${TAGS[@]}\" .\n"+
			`          echo "::error::push failed — retry publishes --tag ${REPO}/${IMAGE}:latest"`+"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "inside a string is refused") {
		t.Fatalf("a --tag inside a message must be refused, got:\n%s", got)
	}
}

func TestAnEchoedShaTagDoesNotSatisfyTheShaArm(t *testing.T) {
	// THE FAIL-OPEN MIRROR, which is the worse half: arm 4 asks whether a `sha-`
	// tag is published outside the gate. If a string can supply one, a workflow
	// that publishes NOTHING on a branch passes it while merely mentioning the tag.
	body := rep(t, good,
		`          for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); done`+"\n",
		`          echo "each build publishes --tag ${REPO}/${IMAGE}:sha-${SHA}"`+"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "outside the `PUBLISH_MUTABLE` gate") {
		t.Fatalf("an echoed sha- tag must not satisfy the sha arm, got:\n%s", got)
	}
}

func TestShortTagOnAContinuationLineIsRefused(t *testing.T) {
	// The real invocation is a backslash continuation, so the flag lands on a line
	// that says nothing about building. Scoping the refusal to a single line made it
	// blind to the only spelling this workflow actually uses.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		"          docker buildx build \\\n"+
			"            --push \\\n"+
			`            -t "${REPO}/${IMAGE}:edge" \`+"\n"+
			"            .\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "short `-t` tag flag is refused") {
		t.Fatalf("a -t on a continuation line must be refused, got:\n%s", got)
	}
}

func TestAttachedShortTagIsRefused(t *testing.T) {
	// docker's flag parser takes `-t<value>` with no space, so requiring one was a
	// bypass of the bypass-refusal.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		`          docker buildx build --push -t"${REPO}/${IMAGE}:edge" .`+"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "short `-t` tag flag is refused") {
		t.Fatalf("an attached -t must be refused, got:\n%s", got)
	}
}

// ── Round four: quoting cuts both ways, and a string can outlive its line ────

func TestAQuotedFlagIsStillAFlag(t *testing.T) {
	// THE FAIL-OPEN SIDE OF "a string is not a publish". Bash does not care where
	// the quotes are: `TAGS+=("--tag" "…:latest")` is the same publish with the flag
	// quoted, and skipping every quoted `--tag` as prose restored #451 straight past
	// the guard built to hold it. A quoted span that is EXACTLY the flag is a flag;
	// the flag inside a sentence is prose.
	body := rep(t, good, `for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); done`,
		`for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); TAGS+=("--tag" "${REPO}/${NAME}:latest"); done`)
	// Judged by the quoted-flag arm now: the publish is real, and the spelling is
	// the one the guard refuses rather than reads.
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "inside a string is refused") {
		t.Fatalf("a quoted --tag must still be judged, got:\n%s", got)
	}
}

func TestAShortTagInAMessageIsRefusedToo(t *testing.T) {
	// Same reversal, same reason: `-t` is refused as a SPELLING rather than parsed,
	// so where it sits in a sentence cannot change the answer.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		"          docker buildx build --push \"${TAGS[@]}\" .\n"+
			`          echo "::error::do not run docker buildx build -t ${REPO}/${IMAGE}:edge by hand"`+"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "short `-t` tag flag is refused") {
		t.Fatalf("a -t in a message must be refused, got:\n%s", got)
	}
}

func TestBareDockerPushOfAMutableTagIsRefused(t *testing.T) {
	// `--tag` is not the only way to publish. `docker push …:latest` carries no flag
	// this guard reads, so it would have been invisible — the same bypass family as
	// `-t`, and refused the same way rather than half-parsed.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		"          docker buildx build --push \"${TAGS[@]}\" .\n"+
			`          docker push "${REPO}/${IMAGE}:latest"`+"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "docker push") {
		t.Fatalf("a bare docker push must be refused, got:\n%s", got)
	}
}

func TestFoldedGateExpressionIsRead(t *testing.T) {
	// The expression is 141 characters; wrapping it in a folded scalar is the
	// obvious tidy-up, and reading only the key's own line captured `>-` and failed
	// on a gate that is completely correct. It is compared with whitespace
	// normalised, so the fold is a re-indent and not a different value.
	body := rep(t, good,
		`  PUBLISH_MUTABLE: ${{ (github.ref == 'refs/heads/main' || github.ref == 'refs/heads/master') && (inputs.sha == '' || inputs.sha == github.sha) }}`+"\n",
		"  PUBLISH_MUTABLE: >-\n"+
			"    ${{ (github.ref == 'refs/heads/main' || github.ref == 'refs/heads/master')\n"+
			"    && (inputs.sha == '' || inputs.sha == github.sha) }}\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("a folded gate expression must be read, got:\n%s", msgs(ps))
	}
}

func TestAStringMayOutliveItsLine(t *testing.T) {
	// Quote state was reset per line, so the second half of a multi-line string was
	// scanned as code: a `fi` in prose popped the gate stack and everything after it
	// stopped being gated.
	body := rep(t, good,
		`            for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:latest"); [ -z "${VERSION}" ] || TAGS+=(--tag "${REPO}/${NAME}:${VERSION}"); done`+"\n",
		"            echo \"publishing the mutable tags now;\n"+
			"            fi is a word that appears in this sentence\"\n"+
			`            for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:latest"); done`+"\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("a multi-line string must not be read as code, got:\n%s", msgs(ps))
	}
}

func TestHeredocFailsClosed(t *testing.T) {
	// A heredoc is a third quoting form this scanner does not model. Reading its
	// body as code is how a `fi` or a `--tag` in a template becomes a verdict, so it
	// refuses rather than guesses.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -euo pipefail\n          cat <<EOF > /tmp/notes\n          --tag ${REPO}/${IMAGE}:latest\n          EOF\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "heredoc") {
		t.Fatalf("a heredoc must fail closed, got:\n%s", got)
	}
}

// ── Round five: a missing tag IS a tag, and a gate can be spelled over lines ──

func TestTaglessShortTagIsRefused(t *testing.T) {
	// docker publishes a reference with no tag as `:latest`. Requiring a `:` before
	// refusing `-t` therefore let the most mutable publish of all through — the one
	// that does not say which tag it moves.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		`          docker buildx build --push "${TAGS[@]}" -t "${REPO}/${IMAGE}" .`+"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "short `-t` tag flag is refused") {
		t.Fatalf("a tagless -t must be refused, got:\n%s", got)
	}
}

func TestTaglessDockerPushIsRefused(t *testing.T) {
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		"          docker buildx build --push \"${TAGS[@]}\" .\n"+
			`          docker push "${REPO}/${IMAGE}"`+"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "docker push") {
		t.Fatalf("a tagless docker push must be refused, got:\n%s", got)
	}
}

func TestNonCanonicalGateSpanningLinesSaysSo(t *testing.T) {
	// The "you wrote a gate I cannot verify" message was decided per LINE, so it
	// only fired when the condition and the tag shared one. A `[[ … == "true" ]]`
	// block — a correct gate this guard still cannot confirm — produced "move it
	// inside the PUBLISH_MUTABLE gate" pointing at lines already inside it.
	body := rep(t, good, `if [ "${PUBLISH_MUTABLE}" = "true" ]; then`, `if [[ "${PUBLISH_MUTABLE}" == "true" ]]; then`)
	got := msgs(judgeBody(t, body))
	if !strings.Contains(got, "canonical") {
		t.Fatalf("a multi-line non-canonical gate must be named as such, got:\n%s", got)
	}
	if strings.Contains(got, "move it inside") {
		t.Fatalf("it must not tell the author to move a tag into the gate it is already in, got:\n%s", got)
	}
}

func TestWritingTheGateToGithubEnvIsRejected(t *testing.T) {
	// `echo "PUBLISH_MUTABLE=true" >> "$GITHUB_ENV"` sets the variable for every
	// LATER step, so the gate reads `true` on any ref while the workflow-level
	// expression above still reads perfectly — the same shadowing the step-env arm
	// catches, through a door YAML cannot see.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -euo pipefail\n          echo \"PUBLISH_MUTABLE=true\" >> \"$GITHUB_ENV\"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "GITHUB_ENV") {
		t.Fatalf("writing the gate variable at runtime must be rejected, got:\n%s", got)
	}
}

func TestHerestringIsNotAHeredoc(t *testing.T) {
	// `<<<` is one word, not a body — the heredoc refusal says so and matched it
	// anyway, because the scan could start on the second `<`.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -euo pipefail\n          grep -q main <<<mainline\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("a herestring must not be read as a heredoc, got:\n%s", msgs(ps))
	}
}

// ── Round six: the expression is a condition too ─────────────────────────────

func TestInvertedYAMLGateIsRejected(t *testing.T) {
	// The shell side goes to some lengths to reject `!=`; the YAML side only asked
	// whether the string "github.ref" appeared anywhere in the expression. So the
	// same inversion, moved one layer up, passed clean: publish the mutable tags
	// from every ref EXCEPT the default branch.
	body := rep(t, good, `github.ref == 'refs/heads/main'`, `github.ref != 'refs/heads/main'`)
	// Anchored on the OFFENDING expression being echoed back, not on the word `!=`:
	// the one generic message names `github.ref !=` among the spellings it refuses,
	// so a bare Contains(got, "!=") would pass on any deviation at all.
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "It is `${{ (github.ref != 'refs/heads/main'") {
		t.Fatalf("an inverted gate expression must be rejected and quoted back, got:\n%s", got)
	}
}

func TestRefNameGateIsRejected(t *testing.T) {
	// `github.ref_name` CONTAINS "github.ref" and is a different context: it is
	// `main`, not `refs/heads/main`, so this gate is false on every ref and quietly
	// stops republishing `:latest` on main — the failure-OPEN direction the workflow
	// comment argues about, waved through by a substring test.
	body := rep(t, good, `github.ref == 'refs/heads/main'`, `github.ref_name == 'main'`)
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "It is `${{ (github.ref_name == 'main'") {
		t.Fatalf("a ref_name gate must be rejected and quoted back, got:\n%s", got)
	}
}

func TestArithmeticShiftIsNotAHeredoc(t *testing.T) {
	// `$((1 << attempt))` is arithmetic, not a heredoc — and this workflow's retry
	// loop already computes its backoff that way.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -euo pipefail\n          sleep $((1 << attempt))\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("arithmetic must not be read as a heredoc, got:\n%s", msgs(ps))
	}
}

func TestTagFlagInAnAssignmentIsAPublish(t *testing.T) {
	// `EXTRA="--tag …:latest"` expanded onto the build line is a publish with the
	// whole flag quoted. A span that BEGINS with the flag is an argument list; the
	// flag in the middle of a sentence is still prose.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		`          EXTRA="--tag ${REPO}/${IMAGE}:latest"`+"\n"+
			"          docker buildx build --push \"${TAGS[@]}\" ${EXTRA} .\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "inside a string is refused") {
		t.Fatalf("a --tag inside an assignment must be judged, got:\n%s", got)
	}
}

func TestConditionSpanningLinesStillNamesTheGate(t *testing.T) {
	// A condition can wrap. Reading only the `if`'s first physical line meant the
	// test that mentions PUBLISH_MUTABLE was invisible, and every tag in the body
	// was told to "move it inside the gate" it was already inside.
	body := rep(t, good, `          if [ "${PUBLISH_MUTABLE}" = "true" ]; then`+"\n",
		"          if [ \"${GITHUB_REF_NAME}\" = \"main\" ] && \\\n"+
			"             [ \"${PUBLISH_MUTABLE}\" = \"yes\" ]\n"+
			"          then\n")
	got := msgs(judgeBody(t, body))
	if !strings.Contains(got, "canonical") {
		t.Fatalf("a wrapped condition naming the gate must be reported as non-canonical, got:\n%s", got)
	}
	if strings.Contains(got, "move it inside") {
		t.Fatalf("it must not send the author into a circle, got:\n%s", got)
	}
}

// ── Round seven: the tag array is where the flags live ───────────────────────

func TestShortTagInTheTagArrayIsRefused(t *testing.T) {
	// `-t` was refused only on a line that BUILDS — but this workflow assembles its
	// flags into TAGS on lines that build nothing, and the build line just expands
	// the array. So `TAGS+=(-t "…:latest")` beside the sha- assembly put #451 back
	// verbatim with every other arm still satisfied by the real gated block.
	body := rep(t, good, `for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); done`,
		`for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); TAGS+=(-t "${REPO}/${NAME}:latest"); done`)
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "short `-t` tag flag is refused") {
		t.Fatalf("a -t in the tag array must be refused, got:\n%s", got)
	}
}

func TestUnbalancedNamesTheOpeningLine(t *testing.T) {
	// "Somewhere in this script" is the least actionable thing a scanner can say
	// about a block it lost. It knows which `if` never closed.
	body := rep(t, good, "          fi\n", "")
	var got problem
	for _, p := range judgeBody(t, body) {
		if strings.Contains(p.msg, "did not close") {
			got = p
		}
	}
	if got.msg == "" {
		t.Fatal("an unbalanced script must still be reported")
	}
	// The fixture's `if` is the 4th line of the run script, which starts at file
	// line 10 — the unclosed block, not the script's first line.
	want := 0
	for i, l := range strings.Split(good, "\n") {
		if strings.Contains(l, `if [ "${PUBLISH_MUTABLE}"`) {
			want = i + 1
		}
	}
	if got.line != want {
		t.Fatalf("want the unclosed `if` at line %d, got line %d (%s)", want, got.line, got.msg)
	}
}

// ── Round eight: escapes, the whole expression, and a fourth publish channel ──

func TestABackslashOutsideQuotesDoesNotOpenAString(t *testing.T) {
	// The shell's escape works everywhere, not only inside double quotes. Honouring
	// it only inside them meant `echo Say \"hello` opened a phantom quoted run that
	// swallowed the REST OF THE SCRIPT — after which an ungated `:latest` was
	// classified as prose and the guard printed OK.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -euo pipefail\n          echo Say \\\"hello\n")
	body = rep(t, body, `          if [ "${PUBLISH_MUTABLE}" = "true" ]; then`+"\n", "")
	body = rep(t, body, "          fi\n", "")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "MUTABLE tag published from any ref") {
		t.Fatalf("an escaped quote must not open a string, got:\n%s", got)
	}
}

func TestNegatedGateExpressionIsRejected(t *testing.T) {
	// `!(github.ref == 'refs/heads/main')` is the inversion again, spelled around
	// the comparison instead of inside it — and a check that only looked for
	// `github.ref ==` and a literal `!=` waved it through.
	body := rep(t, good, `${{ (github.ref == 'refs/heads/main'`, `${{ !(github.ref == 'refs/heads/main'`)
	if ps := judgeBody(t, body); len(ps) == 0 {
		t.Fatal("a negated gate expression must be rejected")
	}
}

func TestTautologousGateExpressionIsRejected(t *testing.T) {
	// `github.ref == github.ref` is `true` with the right words in it — exactly the
	// `PUBLISH_MUTABLE: true` case this arm exists to catch, dressed up.
	body := rep(t, good, `github.ref == 'refs/heads/main'`, `github.ref == github.ref`)
	if ps := judgeBody(t, body); len(ps) == 0 {
		t.Fatal("a tautologous gate expression must be rejected")
	}
}

func TestGateOnSomeOtherBranchIsRejected(t *testing.T) {
	// The rule is the DEFAULT branch, not whichever branch someone was working on.
	body := rep(t, good, `github.ref == 'refs/heads/main'`, `github.ref == 'refs/heads/some-feature'`)
	if ps := judgeBody(t, body); len(ps) == 0 {
		t.Fatal("a gate on a non-default branch must be rejected")
	}
}

func TestDroppingTheShaHalfOfTheGateIsRejected(t *testing.T) {
	// Nothing held the second half. Without it a dispatch on main with `-f
	// sha=<old>` stamps `:latest` with an arbitrary commit — the same broken
	// invariant as a branch build, through the input the workflow already accepts.
	body := rep(t, good, ` && (inputs.sha == '' || inputs.sha == github.sha)`, "")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "'refs/heads/master') }}`; it must be") {
		t.Fatalf("dropping the sha half must be rejected and the truncated expression quoted back, got:\n%s", got)
	}
}

func TestBuildxOutputPublishIsRefused(t *testing.T) {
	// `--output type=image,name=<ref>,push=true` publishes with no `--tag` anywhere
	// on the line — the same bypass family as `-t` and `docker push`, and refused
	// the same way.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		`          docker buildx build --output type=image,name="${REPO}/${IMAGE}:latest",push=true .`+"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "--output") {
		t.Fatalf("a --output publish must be refused, got:\n%s", got)
	}
}

// ── Round nine: stop reading the expression, and stop guessing at `-o` ───────

func TestASecondBranchLiteralIsRejected(t *testing.T) {
	// Adding a literal rather than replacing it: `main || my-feature` publishes the
	// mutable tags from a feature branch, and every pattern-reading of this
	// expression so far has said yes to it.
	body := rep(t, good, `github.ref == 'refs/heads/main'`,
		`(github.ref == 'refs/heads/main' || github.ref == 'refs/heads/my-feature')`)
	if ps := judgeBody(t, body); len(ps) == 0 {
		t.Fatal("a second branch literal must be rejected")
	}
}

func TestOrTrueIsRejected(t *testing.T) {
	// `|| true` is `true` on every ref with the whole correct expression still
	// sitting next to it.
	body := rep(t, good, `github.ref == 'refs/heads/main'`, `(github.ref == 'refs/heads/main' || true)`)
	if ps := judgeBody(t, body); len(ps) == 0 {
		t.Fatal("`|| true` must be rejected")
	}
}

func TestTheShaHalfWithoutItsEmptyCaseIsRejected(t *testing.T) {
	// The shape a maintainer reaches for when the sha arm goes red — and it is FALSE
	// on every push to main, because `inputs` is empty there. `:latest` then stops
	// being republished, silently, which is the failure-open direction.
	body := rep(t, good, `(inputs.sha == '' || inputs.sha == github.sha)`, `(inputs.sha == github.sha)`)
	if ps := judgeBody(t, body); len(ps) == 0 {
		t.Fatal("the sha half without its empty case must be rejected")
	}
}

func TestAnUnrelatedNegationIsNotCalledAnInversion(t *testing.T) {
	// `&& !(github.event_name == 'schedule')` narrows the gate; it does not invert
	// the ref test. Rejecting it is right — it is not the sanctioned expression —
	// but saying it "publishes from every ref EXCEPT the default branch" is a
	// confident sentence about the opposite of what it does.
	body := rep(t, good, `&& (inputs.sha == '' || inputs.sha == github.sha)`,
		`&& !(github.event_name == 'schedule') && (inputs.sha == '' || inputs.sha == github.sha)`)
	got := msgs(judgeBody(t, body))
	if len(got) == 0 {
		t.Fatal("a deviation from the sanctioned expression must be rejected")
	}
	if strings.Contains(got, "EXCEPT the default branch") {
		t.Fatalf("it must not be diagnosed as an inversion, got:\n%s", got)
	}
}

func TestSetOAndCurlOAreNotOutputPublishes(t *testing.T) {
	// `-o` collides with far more commands than `-t` does. `set -o pipefail` is in
	// every script this workflow has.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -o pipefail\n          curl -sSfL -o /tmp/x https://example.invalid/x\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("`set -o` and `curl -o` must not be read as buildx exporters, got:\n%s", msgs(ps))
	}
}

func TestACommentAfterAnEscapedQuoteIsStillAComment(t *testing.T) {
	// stripComment and spansOf have to agree about the escape, or one of them reads
	// a comment as shell and reports a block that never closed.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -euo pipefail\n          echo Say \\\"hello   # publish only if this is main\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("the comment must still be stripped, got:\n%s", msgs(ps))
	}
}

// ── Round ten: the quoted spelling of every flag, and the gate as a variable ──

func TestQuotedOutputFlagIsAPublish(t *testing.T) {
	// `realFlag` exists so a quoted flag is still a flag; the `--output` arm was
	// added without it, and its pattern required whitespace or `(` in front. So the
	// exporter came back through the same door `--tag` had already been fixed for.
	for _, spelling := range []string{
		`          EXTRA="--output type=image,name=${REPO}/${IMAGE}:latest,push=true"`,
		`          ARGS+=("--output" "type=image,name=${REPO}/${IMAGE}:latest,push=true")`,
	} {
		body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
			spelling+"\n          docker buildx build --push \"${TAGS[@]}\" ${EXTRA} .\n")
		if got := msgs(judgeBody(t, body)); !strings.Contains(got, "--output") {
			t.Fatalf("a quoted --output must be refused (%s), got:\n%s", spelling, got)
		}
	}
}

func TestQuotedShortTagFlagIsRefused(t *testing.T) {
	body := rep(t, good, `for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); done`,
		`for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:sha-${SHA}"); TAGS+=("-t" "${REPO}/${NAME}:latest"); done`)
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "short `-t` tag flag is refused") {
		t.Fatalf("a quoted -t must be refused, got:\n%s", got)
	}
}

func TestAssigningTheGateInTheScriptIsRejected(t *testing.T) {
	// The expression is held to one exact value — and a shell assignment in the
	// script overwrites what that value produced, for the rest of the script, with
	// nothing in the YAML to say so. `$GITHUB_ENV` was the door that got closed;
	// this is the one next to it.
	for _, spelling := range []string{`          PUBLISH_MUTABLE=true`, `          export PUBLISH_MUTABLE="true"`} {
		body := rep(t, good, "          set -euo pipefail\n", "          set -euo pipefail\n"+spelling+"\n")
		if got := msgs(judgeBody(t, body)); !strings.Contains(got, "at RUNTIME") {
			t.Fatalf("assigning the gate in the script must be rejected (%s), got:\n%s", spelling, got)
		}
	}
}

func TestAHashInsideAStringIsNotAComment(t *testing.T) {
	// If a quoted `#` started a comment, everything after it on the line would be
	// discarded — including the publish that follows here, which would then trip the
	// "no mutable tag at all" arm instead.
	body := rep(t, good,
		`            for NAME in "${NAMES[@]}"; do TAGS+=(--tag "${REPO}/${NAME}:latest"); [ -z "${VERSION}" ] || TAGS+=(--tag "${REPO}/${NAME}:${VERSION}"); done`,
		`            echo "attempt #1"; TAGS+=(--tag "${REPO}/${IMAGE}:latest")`)
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("a quoted # must not truncate the line, got:\n%s", msgs(ps))
	}
}

func TestDockerImagePushIsRefused(t *testing.T) {
	// `docker image push` is the same command spelled the long way.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		"          docker buildx build --push \"${TAGS[@]}\" .\n"+
			`          docker image push "${REPO}/${IMAGE}:latest"`+"\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "docker push") {
		t.Fatalf("`docker image push` must be refused, got:\n%s", got)
	}
}

// ── Round eleven: a mention must not shadow a publish, or become one ─────────

func TestAnEarlierMentionDoesNotHideALaterPublish(t *testing.T) {
	// Taking the FIRST match on a line and then asking whether it is prose means a
	// sentence about the flag suppresses the real use of it beside it. `--tag`
	// already looped over every match; the refusal arms did not.
	for _, line := range []string{
		`          echo "never use --output type=image,push=true"; docker buildx build --output type=image,name="${REPO}/${IMAGE}:latest",push=true .`,
		`          echo "-t is not how we tag"; docker buildx build --push -t "${REPO}/${IMAGE}:edge" .`,
		`          echo "no docker push here"; docker push "${REPO}/${IMAGE}:latest"`,
	} {
		body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n", line+"\n")
		if ps := judgeBody(t, body); len(ps) == 0 {
			t.Fatalf("a mention must not hide the publish beside it: %s", line)
		}
	}
}

func TestASentenceNamingAFlagIsRefused(t *testing.T) {
	// The third of the inverted three. Every one of these is a correct sentence and
	// every one of them is now a finding, because the alternative — deciding which
	// sentences are really arguments — is what produced four rounds of alternating
	// fail-open and false-positive. The remedy is in the message.
	for _, line := range []string{
		`          echo "-t is banned in this workflow"`,
		`          echo "--tag is assembled into TAGS, never passed by hand"`,
	} {
		body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
			line+"\n          docker buildx build --push \"${TAGS[@]}\" .\n")
		if ps := judgeBody(t, body); len(ps) == 0 {
			t.Fatalf("a sentence naming a publish flag must be refused: %s", line)
		}
	}
}

func TestMentioningTheGateIsNotAssigningIt(t *testing.T) {
	// An assignment is a command; `PUBLISH_MUTABLE=` in the middle of one is an
	// argument to something else. Printing the value and grepping for it are both
	// ordinary, and neither changes the gate.
	for _, line := range []string{
		`          echo PUBLISH_MUTABLE="${PUBLISH_MUTABLE}"`,
		`          env | grep PUBLISH_MUTABLE=`,
	} {
		body := rep(t, good, "          set -euo pipefail\n", "          set -euo pipefail\n"+line+"\n")
		if ps := judgeBody(t, body); len(ps) != 0 {
			t.Fatalf("mentioning the gate must not read as setting it (%s), got:\n%s", line, msgs(ps))
		}
	}
}

// ── Round twelve: a value is not always value-shaped ─────────────────────────

func TestQuotedDockerPushIsStillRefused(t *testing.T) {
	// The value-shape rule was written for FLAGS and then applied to the `docker`
	// COMMAND match too: `docker push ${REPO}/x:latest` has `push` as its second
	// word, which is not reference-shaped, so the whole publish was reclassified as
	// prose. It is a regression of the exact bypass family this branch is about.
	for _, line := range []string{
		`          eval "docker push ${REPO}/${IMAGE}:latest"`,
		`          sh -c "docker image push ${REPO}/${IMAGE}:latest"`,
	} {
		body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
			"          docker buildx build --push \"${TAGS[@]}\" .\n"+line+"\n")
		if got := msgs(judgeBody(t, body)); !strings.Contains(got, "docker push") {
			t.Fatalf("a quoted docker push must be refused (%s), got:\n%s", line, got)
		}
	}
}

func TestQuotedTaglessShortTagIsStillRefused(t *testing.T) {
	// `EXTRA="-t myimage"` — the untagged spelling this guard calls the most mutable
	// publish there is — has a second word that is a bare name, so the same rule
	// dropped it.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		`          EXTRA="-t myimage"`+"\n          docker buildx build --push \"${TAGS[@]}\" ${EXTRA} .\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "short `-t` tag flag is refused") {
		t.Fatalf("a quoted tagless -t must be refused, got:\n%s", got)
	}
}

func TestGrepDashOIsNotAnExporter(t *testing.T) {
	// The exporter is matched on its VALUE, but "contains type=image" is not the
	// same as "is an exporter argument": `grep -o type=image` and a jsonpath both
	// carry the substring and neither publishes anything.
	for _, line := range []string{
		`          grep -o type=image manifest.txt`,
		`          kubectl get pod -o jsonpath={.type=image}`,
	} {
		body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
			"          docker buildx build --push \"${TAGS[@]}\" .\n"+line+"\n")
		if ps := judgeBody(t, body); len(ps) != 0 {
			t.Fatalf("`%s` must not be read as a buildx exporter, got:\n%s", line, msgs(ps))
		}
	}
}

func TestEveryDeclarationOfTheGateIsRejected(t *testing.T) {
	// `export` is one of five ways to spell the same assignment, and `eval` hides a
	// sixth inside a string the structural scan blanks.
	for _, line := range []string{
		`          readonly PUBLISH_MUTABLE=true`,
		`          declare PUBLISH_MUTABLE=true`,
		`          typeset PUBLISH_MUTABLE=true`,
		`          local PUBLISH_MUTABLE=true`,
		`          eval "PUBLISH_MUTABLE=true"`,
	} {
		body := rep(t, good, "          set -euo pipefail\n", "          set -euo pipefail\n"+line+"\n")
		if got := msgs(judgeBody(t, body)); !strings.Contains(got, "at RUNTIME") {
			t.Fatalf("`%s` must be rejected, got:\n%s", line, got)
		}
	}
}

// ── Round thirteen: stop classifying quoted flags, and refuse them ───────────
//
// FOUR ROUNDS TRIED TO TELL A QUOTED ARGUMENT LIST FROM A SENTENCE and each rule
// had a next spelling: content == flag; begins-with; begins-with plus every later
// word "value-shaped"; the same plus a list of subcommands. Every widening let a
// real publish through (`EXTRA="--tag …:latest --target runtime"`,
// `eval "docker push … && echo ok"`) or turned a message into one. The question is
// undecidable at this altitude, so the guard stops asking it: a publish flag
// inside a string is REFUSED, exactly as `-t`, `docker push` and heredocs already
// are. The rule is satisfiable in one edit and it has no next spelling.

func TestAnyQuotedPublishFlagIsRefused(t *testing.T) {
	for _, line := range []string{
		`          EXTRA="--tag ${REPO}/${IMAGE}:latest --target runtime"`,
		`          EXTRA="--tag ${REPO}/${IMAGE}:latest"`,
		`          eval "docker push ${REPO}/${IMAGE}:latest && echo pushed"`,
		`          sh -c "docker push ${REPO}/${IMAGE}:latest || exit 1"`,
		`          ARGS+=("--output" "type=image,name=${REPO}/${IMAGE}:latest,push=true")`,
		`          EXTRA="-t myimage --builder default"`,
		`          echo "::error::push failed — retry publishes --tag ${REPO}/${IMAGE}:latest"`,
	} {
		body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
			line+"\n          docker buildx build --push \"${TAGS[@]}\" ${EXTRA} .\n")
		if ps := judgeBody(t, body); len(ps) == 0 {
			t.Fatalf("a quoted publish flag must be refused: %s", line)
		}
	}
}

func TestABraceGroupAssignmentIsRejected(t *testing.T) {
	// `{ PUBLISH_MUTABLE=true; }` is a command group, and this workflow already
	// writes `{ echo "::error::…"; exit 1; }` — the nearest spelling to hand.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -euo pipefail\n          { PUBLISH_MUTABLE=true; }\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "at RUNTIME") {
		t.Fatalf("a brace-group assignment must be rejected, got:\n%s", got)
	}
}

func TestAnOutputFlagSplitOverLinesFailsClosed(t *testing.T) {
	// The workflow's build command is written one flag per line with backslashes,
	// which is the layout in which the exporter's value is NOT on the flag's line.
	// Unreadable is a failure here, like every other unreadable form.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		"          docker buildx build \\\n            --output \\\n"+
			`            type=image,name="${REPO}/${IMAGE}:latest",push=true \`+"\n            .\n")
	if ps := judgeBody(t, body); len(ps) == 0 {
		t.Fatal("an --output whose value is on the next line must not pass")
	}
}

func TestMentioningGithubEnvInProseIsNotAWrite(t *testing.T) {
	// The clause was two unanchored Contains while its comment claimed it was
	// "pinned to the redirect". A write has a redirect; a sentence does not.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -euo pipefail\n          echo \"see GITHUB_ENV docs before touching PUBLISH_MUTABLE\"\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("prose naming both must not read as a write, got:\n%s", msgs(ps))
	}
}

// ── Round fourteen: a command substitution is code, not a string ─────────────

func TestCommandSubstitutionInsideQuotesIsCode(t *testing.T) {
	// `"$( … )"` is quoted, but its CONTENTS are a command. Blanking them lost the
	// exemption words, so `TMP="$(mktemp -t llz.XXXX)"` was refused as a docker tag
	// while the unquoted spelling passed — a distinction the shell does not make.
	for _, line := range []string{
		`          TMP="$(mktemp -d -t llz.XXXX)"`,
		`          GOT="$(docker run --rm -t "${REPO}/${IMAGE}:sha-${SHA}" version)"`,
	} {
		body := rep(t, good, "          set -euo pipefail\n", "          set -euo pipefail\n"+line+"\n")
		if ps := judgeBody(t, body); len(ps) != 0 {
			t.Fatalf("a command substitution must be read as code (%s), got:\n%s", line, msgs(ps))
		}
	}
}

func TestEveryRedirectIntoGithubEnvIsAWrite(t *testing.T) {
	// Pinning the clause to the redirect was right; pinning it to `>>` alone was
	// narrower than the two Contains it replaced.
	for _, line := range []string{
		`          echo "PUBLISH_MUTABLE=true" > "$GITHUB_ENV"`,
		`          echo "PUBLISH_MUTABLE=true" | tee -a "$GITHUB_ENV"`,
	} {
		body := rep(t, good, "          set -euo pipefail\n", "          set -euo pipefail\n"+line+"\n")
		if got := msgs(judgeBody(t, body)); !strings.Contains(got, "at RUNTIME") {
			t.Fatalf("`%s` must be rejected, got:\n%s", line, got)
		}
	}
}

func TestAnExporterValueInAVariableFailsClosed(t *testing.T) {
	// `--tag "$LATEST"` already fails closed as an unreadable reference; the
	// exporter's value has to be held to the same rule or it is the way round it.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		`          OUT="type=image,name=${REPO}/${IMAGE}:latest,push=true"`+"\n"+
			"          docker buildx build --output \"$OUT\" .\n")
	if ps := judgeBody(t, body); len(ps) == 0 {
		t.Fatal("an exporter value in a variable must not pass")
	}
}

func TestUnrelatedDashOFlagsAreNotExporters(t *testing.T) {
	// The fail-closed branch fired whenever the value was unreadable, whatever the
	// command — so a curl continuation, a trailing `-o`, and `--output-dir` all
	// reported a buildx exporter publish.
	for _, line := range []string{
		"          curl -fsSL -o \\\n            /tmp/x https://example.invalid/x",
		`          jq -r '.a' f -o`,
		`          helm template x --output-dir /tmp/out`,
	} {
		body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
			line+"\n          docker buildx build --push \"${TAGS[@]}\" .\n")
		if ps := judgeBody(t, body); len(ps) != 0 {
			t.Fatalf("`%s` must not read as an exporter, got:\n%s", line, msgs(ps))
		}
	}
}

// ── Round fifteen: the exemptions belong to a command, not to a line ─────────

func TestASubstitutionDoesNotExemptTheBuildsOwnTag(t *testing.T) {
	// Un-blanking `$( … )` put its words into the text the exemptions read, and the
	// exemption test was line-wide — so a build stamping a date exempted its own
	// `-t`. A fail-open introduced by the fix for a false positive.
	for _, line := range []string{
		`          docker buildx build -t "${REPO}/${IMAGE}:latest" --build-arg BUILD_DATE="$(date -u +%FT%TZ)" --push .`,
		`          docker buildx build -t "${REPO}/${IMAGE}:latest" --build-arg TMP="$(mktemp -d)" --push .`,
	} {
		body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n", line+"\n")
		if got := msgs(judgeBody(t, body)); !strings.Contains(got, "short `-t` tag flag is refused") {
			t.Fatalf("a build's own -t must never be exempted (%s), got:\n%s", line, got)
		}
	}
}

func TestQuotesInsideASubstitutionAreTracked(t *testing.T) {
	// The `$(`-skip counted parens with no idea of quoting, so a paren inside a
	// string never balanced and the scan ran to end of line — leaving every later
	// line marked as string content.
	for _, line := range []string{
		`          NAME="$(echo "$X" | tr -d '(')"`,
		`          N="$(grep -c ' # tag' Dockerfile)"`,
	} {
		body := rep(t, good, "          set -euo pipefail\n", "          set -euo pipefail\n"+line+"\n")
		if ps := judgeBody(t, body); len(ps) != 0 {
			t.Fatalf("a substitution must not swallow the rest of the file (%s), got:\n%s", line, msgs(ps))
		}
	}
}

func TestReadSetsTheGateToo(t *testing.T) {
	// `read` assigns without an `=`.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -euo pipefail\n          read -r PUBLISH_MUTABLE <<< true\n")
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "at RUNTIME") {
		t.Fatalf("`read` into the gate must be rejected, got:\n%s", got)
	}
}

func TestAnUnrelatedDashOOnABuildLineIsNotAnExporter(t *testing.T) {
	// Failing closed on an unreadable `-o` is right for the BUILD's own flags; the
	// command after `&&` is a different command, and `sort -o "$OUT"` is not an
	// exporter however the line began.
	body := rep(t, good, "          docker buildx build --push \"${TAGS[@]}\" .\n",
		`          docker buildx build --push "${TAGS[@]}" . && sort -o "$OUT" f`+"\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("a -o in another command must not read as an exporter, got:\n%s", msgs(ps))
	}
}

func TestNestedSubstitutionsAndTheirReopenedRun(t *testing.T) {
	// Nesting exercises the depth counter in both directions, and the closing paren
	// has to hand the double-quoted run back — otherwise the tag after it on the
	// same line is read as code that is not there, or as string that it is not.
	body := rep(t, good, "          set -euo pipefail\n",
		"          set -euo pipefail\n"+
			`          D="$(dirname "$(readlink -f "${0}")")"; echo "${D}"`+"\n")
	if ps := judgeBody(t, body); len(ps) != 0 {
		t.Fatalf("nested substitutions must scan cleanly, got:\n%s", msgs(ps))
	}
}

func TestRunPrintsTheProblemsItFound(t *testing.T) {
	// Run's reporting half — the annotations, the summary and the remedy — is what
	// an operator actually meets, and it had been exercised only on the OK path.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := rep(t, good, `          if [ "${PUBLISH_MUTABLE}" = "true" ]; then`+"\n", "")
	broken = rep(t, broken, "          fi\n", "")
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(publisherFile)), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := Run(dir, &out, &errOut)
	if err == nil {
		t.Fatal("a workflow publishing :latest from any ref must fail")
	}
	for _, want := range []string{"::error file=" + publisherFile, "MUTABLE tag published from any ref", "sha- tag"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("the report must contain %q, got:\n%s", want, errOut.String())
		}
	}
}
