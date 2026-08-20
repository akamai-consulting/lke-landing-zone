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
	return judge(body, tagSites(body))
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
	sites := tagSites(string(body))
	if len(sites) == 0 {
		t.Fatal("the live workflow published no --tag — the fixture-free half of this test would be vacuous")
	}
	if ps := judge(string(body), sites); len(ps) != 0 {
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
