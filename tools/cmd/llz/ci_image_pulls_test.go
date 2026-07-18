package main

import (
	"strings"
	"testing"
)

func TestParseImagePulls(t *testing.T) {
	events := `{"items":[
	  {"message":"Successfully pulled image \"quay.io/argoproj/argocd:v2.13\" in 1m2.5s (1m2.5s including waiting)","involvedObject":{"namespace":"argocd","name":"argocd-server-0"}},
	  {"message":"Successfully pulled image \"ghcr.io/kyverno/kyverno:v1.13\" in 4.2s","involvedObject":{"namespace":"kyverno","name":"kyverno-admission-abc"}},
	  {"message":"some unrelated event with no duration","involvedObject":{"namespace":"x","name":"y"}}
	]}`
	pulls := parseImagePulls([]byte(events))
	if len(pulls) != 2 {
		t.Fatalf("parsed %d pulls, want 2 (the no-duration line skipped)", len(pulls))
	}
	// Sorted slowest-first.
	if pulls[0].Image != "quay.io/argoproj/argocd:v2.13" || int(pulls[0].DurationS) != 62 {
		t.Errorf("slowest pull = %+v, want argocd/62s", pulls[0])
	}
	if pulls[1].Namespace != "kyverno" || pulls[1].DurationS != 4.2 {
		t.Errorf("second pull = %+v, want kyverno/4.2s", pulls[1])
	}

	table := renderImagePullTable(pulls)
	if !strings.Contains(table, "argocd") || !strings.Contains(table, "sum of pulls") {
		t.Errorf("table missing image/sum: %s", table)
	}
}

func TestParseImagePullsEmptyAndMalformed(t *testing.T) {
	if got := parseImagePulls([]byte(`{"items":[]}`)); len(got) != 0 {
		t.Errorf("no events → %d pulls, want 0", len(got))
	}
	if got := parseImagePulls([]byte(`not json`)); got != nil {
		t.Errorf("malformed JSON → %v, want nil", got)
	}
	if s := renderImagePullTable(nil); !strings.Contains(s, "no Pulled events") {
		t.Errorf("empty render should note no events, got %s", s)
	}
}

func TestImageFromPullMessage(t *testing.T) {
	if got := imageFromPullMessage(`Successfully pulled image "x/y:1" in 2s`); got != "x/y:1" {
		t.Errorf("got %q, want x/y:1", got)
	}
	if got := imageFromPullMessage("no quotes here"); got != "" {
		t.Errorf("unquoted → %q, want empty", got)
	}
}
