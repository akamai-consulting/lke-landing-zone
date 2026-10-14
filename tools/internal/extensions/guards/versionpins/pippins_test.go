package versionpins

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// repoForTest reads the REAL checked-in tree, through the same fenced reader the
// gate uses in production.
func repoForTest(t *testing.T) capability.Repo {
	t.Helper()
	return capability.RepoForGate(Extension(), "../../../../..")
}

// pinsFrom runs the REAL collector over a Dockerfile body, through a temp tree.
//
// NOT A REIMPLEMENTATION OF THE SCAN. A helper that re-runs the regex itself keeps
// passing when collectPipPins grows the pip-install line
// scoping the regex now depends on — a test that restates the code under test
// agrees with the version of it that lived in the test.
func pinsFrom(t *testing.T, body string) []pipPin {
	t.Helper()
	pins, err := collectPipPins(pipRepo(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return pins
}

func TestPipPinsCatchAMajorSkewAcrossStages(t *testing.T) {
	// The failure this exists for: copier pinned once per stage, held together by
	// a comment. ci-tofu's copy renders the template for the automated upgrade,
	// the devcontainer's for the local `llz upgrade` that reproduces it — a major
	// between them makes the two legitimately differ, and the PR the bot opens is
	// then unreproducible by the person reviewing it.
	body := `FROM x AS ci-tofu
RUN uv pip install --system "checkov>=3.2.0,<4.0.0" "copier>=9.4,<10"
FROM y AS devcontainer
RUN uv pip install --system "copier>=10,<11" "linode-cli>=5.0"
`
	bad := disagreeingPipPins(pinsFrom(t, body))
	if len(bad) != 1 {
		t.Fatalf("expected the copier skew to be reported once, got %d: %+v", len(bad), bad)
	}
	if bad[0].line != 4 || !strings.Contains(bad[0].what, "copier") {
		t.Errorf("the report must name the second site: %+v", bad[0])
	}
	if bad[0].want != ">=9.4,<10" {
		t.Errorf("the first pin is what the second is measured against, got %q", bad[0].want)
	}
}

func TestPipPinsAcceptAgreementAndDoNotInventDrift(t *testing.T) {
	// A package pinned identically in both stages is the normal state, and a
	// package pinned in only one stage is not drift either.
	body := `FROM x AS ci-tofu
RUN uv pip install "checkov>=3.2.0,<4.0.0" "copier>=9.4,<10"
FROM y AS devcontainer
RUN uv pip install "copier>=9.4,<10" "linode-cli>=5.0" "checkov>=3.2.0,<4.0.0"
`
	pins := pinsFrom(t, body)
	if len(pins) != 5 {
		t.Fatalf("expected 5 requirements, got %d: %+v", len(pins), pins)
	}
	if bad := disagreeingPipPins(pins); len(bad) != 0 {
		t.Errorf("agreeing pins must not be reported: %+v", bad)
	}
}

func TestPipPinsIgnoreComments(t *testing.T) {
	// The prose above the install block quotes the very range it is explaining,
	// and a comment that reads as a second pin would make the gate cry drift at
	// its own rationale.
	body := `FROM x AS ci-tofu
# PINNED TO THE SAME RANGE AS THE DEVCONTAINER ("copier>=1.0,<2") on purpose.
RUN uv pip install "copier>=9.4,<10"
FROM y AS devcontainer
RUN uv pip install "copier>=9.4,<10"
`
	if bad := disagreeingPipPins(pinsFrom(t, body)); len(bad) != 0 {
		t.Errorf("a pin quoted in a comment is prose, not a restatement: %+v", bad)
	}
}

func TestPipPinScanIsNotVacuous(t *testing.T) {
	// The real Dockerfile must contain pins the scanner can see. A regex that
	// stopped matching would report "all agree" having read nothing — which is
	// why Run fails closed on a zero count, and why this asserts against the
	// checked-in file rather than a fixture.
	pins, err := collectPipPins(repoForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) == 0 {
		t.Fatal("the scan found no pip requirements in the real Dockerfile; it is examining nothing")
	}
	seen := map[string]int{}
	for _, p := range pins {
		seen[p.pkg]++
	}
	if seen["copier"] < 2 {
		t.Errorf("copier must be pinned in BOTH the ci-tofu and devcontainer stages — the automated "+
			"upgrade renders the template with the first and the operator reproduces it with the "+
			"second; got %d occurrence(s)", seen["copier"])
	}
}

// pipRepo builds a one-file tree whose Dockerfile carries `body`, read through
// the same fenced reader production uses.
func pipRepo(t *testing.T, body string) capability.Repo {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "dockerfiles/Dockerfile"), body)
	return capability.RepoForGate(Extension(), root)
}

func TestRunPipPinsReportsTheSkewAndNamesTheConsequence(t *testing.T) {
	// The operator-facing half. A gate that detects drift and then prints a bare
	// "mismatch" makes the reader rediscover why it matters; the remedy has to say
	// what a copier major actually costs, which is an upgrade PR the reviewer
	// cannot reproduce locally.
	var out, errOut bytes.Buffer
	n, err := runPipPins(pipRepo(t, `FROM x AS ci-tofu
RUN uv pip install "copier>=9.4,<10"
FROM y AS devcontainer
RUN uv pip install "copier>=10,<11"
`), true, &out, &errOut)
	if err == nil {
		t.Fatal("a skew across stages must fail the gate")
	}
	if n != 2 {
		t.Errorf("both pins must be counted, got %d", n)
	}
	e := errOut.String()
	if !strings.Contains(e, "::error file=dockerfiles/Dockerfile,line=4") {
		t.Errorf("the annotation must point at the disagreeing line, got:\n%s", e)
	}
	if !strings.Contains(e, "llz-template-upgrade.yml") || !strings.Contains(e, "reproduce") {
		t.Errorf("the remedy must name what the skew costs, got:\n%s", e)
	}
	if !strings.Contains(out.String(), "pip   dockerfiles/Dockerfile:2  copier>=9.4,<10") {
		t.Errorf("--verbose must list every pin it read, got:\n%s", out.String())
	}
}

func TestRunPipPinsIsSilentlyFineWithNoPython(t *testing.T) {
	// A Dockerfile that installs nothing with pip legitimately has no pins. Keying
	// the vacuity doctrine on the COUNT rather than on the invocation reds every
	// such tree — which is what it did to five fixtures in this package.
	var out, errOut bytes.Buffer
	n, err := runPipPins(pipRepo(t, "FROM x AS only\nRUN apt-get install -y curl\n"), false, &out, &errOut)
	if err != nil {
		t.Fatalf("a tree with no pip install must pass, got %v", err)
	}
	if n != 0 {
		t.Errorf("nothing to count, got %d", n)
	}
}

func TestRunPipPinsFailsClosedWhenItGoesBlind(t *testing.T) {
	// `pip install` present and zero pins matched means the scanner stopped
	// working, and "all agree" over nothing read is the outage it exists to catch.
	var out, errOut bytes.Buffer
	_, err := runPipPins(pipRepo(t, "FROM x AS only\nRUN uv pip install requests\n"), false, &out, &errOut)
	if err == nil {
		t.Fatal("an unpinned pip install must not read as agreement")
	}
	if !strings.Contains(err.Error(), "examined nothing") {
		t.Errorf("the error must say the scan was vacuous, got %v", err)
	}
	if !strings.Contains(errOut.String(), "::error file=dockerfiles/Dockerfile") {
		t.Errorf("the vacuous scan must be annotated, got:\n%s", errOut.String())
	}
}

func TestPipPinReadErrorsSurface(t *testing.T) {
	// A missing Dockerfile must be an error, not an empty pin list that passes.
	empty := capability.RepoForGate(Extension(), t.TempDir())
	if _, err := collectPipPins(empty); err == nil {
		t.Error("a missing Dockerfile must not read as zero pins")
	}
	if _, err := pipInstallsPresent(empty); err == nil {
		t.Error("a missing Dockerfile must not read as 'no pip installs'")
	}
}

func TestPipPinsSeeThroughTheQuotingStyle(t *testing.T) {
	// Quoting is a formatting choice; it must not decide whether a pin is measured.
	// Matching only double quotes left a place for the drift to hide that costs
	// nothing to reach: single-quote one stage and the scanner sees ONE copier,
	// which it can never compare against anything — green, two stages a major apart.
	body := `FROM x AS ci-tofu
RUN uv pip install --system "copier>=9.4,<10"
FROM y AS devcontainer
RUN uv pip install --system 'copier>=10,<11'
`
	pins := pinsFrom(t, body)
	if len(pins) != 2 {
		t.Fatalf("both quoting styles must be read, got %d: %+v", len(pins), pins)
	}
	if bad := disagreeingPipPins(pins); len(bad) != 1 {
		t.Errorf("a skew hidden behind a different quote must still be reported, got %+v", bad)
	}
}

func TestPipPinsDoNotMistakeArgsAndFlagsForRequirements(t *testing.T) {
	// The price of being loose about quotes is that the REGION has to be tight.
	// Relaxed over the whole file, the pattern swept up `ARG TOFU_VERSION=1.12.5`
	// and `--strip-components=1` and reported 22 requirements where there are 5.
	body := `FROM x AS toolbox
ARG TOFU_VERSION=1.12.5
RUN curl -L http://x | tar -xz --strip-components=1
ENV CGO_ENABLED=0
FROM y AS ci-tofu
RUN uv pip install --system "copier>=9.4,<10"
`
	pins := pinsFrom(t, body)
	if len(pins) != 1 {
		t.Fatalf("only the pip requirement is a requirement, got %d: %+v", len(pins), pins)
	}
	if pins[0].pkg != "copier" {
		t.Errorf("got %+v", pins[0])
	}
}

func TestPipPinsCompareTwoRequirementsOnOneLine(t *testing.T) {
	// The line number is not a unique key for a pin. Using it as the first-pin
	// identity made two conflicting pins of one package on a SINGLE line compare
	// equal to each other and vanish.
	body := `FROM x AS ci-tofu
RUN uv pip install "copier>=9.4,<10" "copier>=10,<11"
`
	if bad := disagreeingPipPins(pinsFrom(t, body)); len(bad) != 1 {
		t.Errorf("two conflicting pins on one line must still disagree, got %+v", bad)
	}
}

func TestPipPinsFollowLineContinuations(t *testing.T) {
	// This repo writes one requirement per continued line, so a scan that stopped
	// at the `pip install` line itself would read none of them.
	body := `FROM x AS ci-tofu
RUN uv pip install --system --no-cache \
      "checkov>=3.2.0,<4.0.0" \
      "copier>=9.4,<10"
RUN echo "unrelated>=1.0"
`
	pins := pinsFrom(t, body)
	if len(pins) != 2 {
		t.Fatalf("continued requirement lines must be read, and only those, got %d: %+v", len(pins), pins)
	}
}
