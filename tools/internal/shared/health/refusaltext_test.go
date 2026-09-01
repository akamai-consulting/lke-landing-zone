package health

// refusaltext_test.go pins how a refusal is rendered for a report line.
//
// WHY IT IS NOT FirstLine. A denial's REASON is never on line one — kubectl
// prints `admission webhook "…" denied the request:` and the policy, the rule and
// the message below it. So the arm that exists to "say what happened and let the
// reader judge" was printing the header and discarding everything worth judging,
// which is the state that arm's own comment records an earlier version being in.

import (
	"strings"
	"testing"
)

func TestRefusalTextKeepsTheReasonAndNotJustTheHeader(t *testing.T) {
	// The real Kyverno shape: the reason is four lines down.
	msg := `Error from server: admission webhook "validate.kyverno.svc-fail" denied the request:

resource StatefulSet/monitoring/loki-ingester was blocked due to the following policies

require-storage-immutability:
  autogen-check-sc: 'validation error: storageClassName cannot be updated.'`
	got := RefusalText(msg)
	for _, want := range []string{"denied the request", "require-storage-immutability", "storageClassName cannot be updated"} {
		if !strings.Contains(got, want) {
			t.Errorf("RefusalText dropped %q — the reader is told to judge a refusal and shown none "+
				"of it.\ngot:%s", want, got)
		}
	}
	// The contrast that motivates the whole function.
	if strings.Contains(FirstLine(msg), "storageClassName") {
		t.Fatal("FirstLine now keeps the reason, so RefusalText has no reason to exist — check " +
			"which of the two changed")
	}
}

func TestRefusalTextIndentsAMultiLineRefusalAsOneBlock(t *testing.T) {
	got := RefusalText("line one\nline two")
	if !strings.HasPrefix(got, "\n      ") {
		t.Errorf("a multi-line refusal is not indented as a block, so it runs into the report line "+
			"that introduces it: %q", got)
	}
	if strings.Count(got, "\n      ") != 2 {
		t.Errorf("not every line is indented: %q", got)
	}
}

func TestRefusalTextLeavesASingleLineAlone(t *testing.T) {
	// One line needs no block treatment, and adding one would push every ordinary
	// refusal onto its own line for no gain.
	if got := RefusalText("statefulsets.apps \"x\" is forbidden"); got != `statefulsets.apps "x" is forbidden` {
		t.Errorf("a single-line refusal was reformatted: %q", got)
	}
}

func TestRefusalTextIsBounded(t *testing.T) {
	// A pathological message must not bury the report it appears in.
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, "line")
	}
	got := RefusalText(strings.Join(many, "\n"))
	if n := strings.Count(got, "line"); n > refusalMaxLines {
		t.Errorf("kept %d lines, want at most %d", n, refusalMaxLines)
	}
	if !strings.Contains(got, "truncated") {
		t.Error("a truncated refusal does not say it was truncated, so a reader cannot tell the " +
			"apiserver stopped talking from this function stopping listening")
	}
	long := strings.Repeat("x", firstLineMax+50)
	if got := RefusalText(long); !strings.HasSuffix(got, "…") || len(got) > firstLineMax+10 {
		t.Errorf("an over-long single line was not capped: len=%d", len(got))
	}
}

func TestRefusalTextHandlesEmptyAndBlankInput(t *testing.T) {
	// The caller reaches this arm precisely when something went wrong, so "" must
	// not render as an empty gap after "the apiserver said:".
	for _, in := range []string{"", "   ", "\n\n\n"} {
		if got := RefusalText(in); got != "(the apiserver said nothing)" {
			t.Errorf("RefusalText(%q) = %q, want an explicit statement that there was no text", in, got)
		}
	}
}

func TestRefusalTextNormalisesCRLF(t *testing.T) {
	got := RefusalText("first\r\nsecond")
	if strings.Contains(got, "\r") {
		t.Errorf("carriage returns survive into the report and render as stray characters: %q", got)
	}
	if !strings.Contains(got, "second") {
		t.Errorf("a CRLF refusal lost its second line: %q", got)
	}
}
