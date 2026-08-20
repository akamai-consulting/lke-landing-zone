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
  PUBLISH_MUTABLE: ${{ (github.ref == 'refs/heads/main') && (inputs.sha == '' || inputs.sha == github.sha) }}
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
		`  PUBLISH_MUTABLE: ${{ (github.ref == 'refs/heads/main') && (inputs.sha == '' || inputs.sha == github.sha) }}`,
		`  PUBLISH_MUTABLE: true`)
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "does not consult `github.ref`") {
		t.Fatalf("a gate that ignores the ref must be rejected, got:\n%s", got)
	}
}

func TestMissingGateEntryIsRejected(t *testing.T) {
	body := rep(t, good,
		`  PUBLISH_MUTABLE: ${{ (github.ref == 'refs/heads/main') && (inputs.sha == '' || inputs.sha == github.sha) }}`+"\n",
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
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "MUTABLE tag published from any ref") {
		t.Fatalf("an inverted gate must be rejected, got:\n%s", got)
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
	if got := msgs(judgeBody(t, body)); !strings.Contains(got, "does not consult `github.ref`") {
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
