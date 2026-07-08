package main

import "testing"

func TestParseJobStatus(t *testing.T) {
	tests := []struct {
		in               string
		wantSucc, wantFl bool
	}{
		{"1/", true, false},      // succeeded
		{"/1", false, true},      // failed
		{"0/0", false, false},    // still running
		{"/", false, false},      // neither set yet
		{"", false, false},       // empty (job just created)
		{" 1 / 0 ", true, false}, // whitespace tolerant
		{"1/1", true, true},      // both (caller prefers succeeded)
	}
	for _, tc := range tests {
		succ, fl := parseJobStatus(tc.in)
		if succ != tc.wantSucc || fl != tc.wantFl {
			t.Errorf("parseJobStatus(%q) = (%v,%v), want (%v,%v)", tc.in, succ, fl, tc.wantSucc, tc.wantFl)
		}
	}
}

func TestParseRotationAction(t *testing.T) {
	// A real run: masked-token warning on stderr, then the JSON audit record.
	logs := `::add-mask::redacted
{"action":"rotated","event":"broad-pat-rotator","new_pat_id":123,"published_envs":["infra-e2e"]}`
	if a, ok := parseRotationAction(logs); !ok || a != "rotated" {
		t.Errorf("parseRotationAction = (%q,%v), want (rotated,true)", a, ok)
	}

	// A not-due tick still emits a record — action=skip must be reported (caller fails on it).
	if a, ok := parseRotationAction(`{"event":"broad-pat-rotator","action":"skip"}`); !ok || a != "skip" {
		t.Errorf("skip record: got (%q,%v)", a, ok)
	}

	// No audit record at all (pod never ran / crashed before printing).
	if _, ok := parseRotationAction("error: something\nboom\n"); ok {
		t.Error("expected no action from logs without an audit record")
	}

	// A different JSON event line must not be mistaken for the rotation record.
	if _, ok := parseRotationAction(`{"event":"other","action":"rotated"}`); ok {
		t.Error("must only match event=broad-pat-rotator")
	}
}
