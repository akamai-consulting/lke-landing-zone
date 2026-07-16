package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestForceBroadPATRotationDue guards the reused-cluster fix: the exercise must
// reset rotated_at=0 via `kv patch` (preserving the minting token) so a cluster
// whose broad PAT was rotated < threshold ago still reads as DUE. Regression for
// the e2e "asserted action=rotated, got skip (not due)" failure on a reused cluster.
func TestForceBroadPATRotationDue(t *testing.T) {
	// No root token → error, and it must NOT attempt the exec.
	t.Setenv("OPENBAO_ROOT_TOKEN", "")
	called := false
	orig := baoExecFn
	baoExecFn = func(_, _, _ string, _ ...string) (string, string, error) { called = true; return "", "", nil }
	t.Cleanup(func() { baoExecFn = orig })
	if err := forceBroadPATRotationDue(); err == nil {
		t.Fatal("expected an error when OPENBAO_ROOT_TOKEN is unset")
	}
	if called {
		t.Fatal("must not exec into OpenBao without a root token")
	}

	// With a token, it patches ONLY rotated_at=0 at the broad-pat path (patch, not
	// put — a put would drop the minting token).
	t.Setenv("OPENBAO_ROOT_TOKEN", "roottoken")
	var gotPod, gotToken string
	var gotArgs []string
	baoExecFn = func(pod, token, _ string, args ...string) (string, string, error) {
		gotPod, gotToken, gotArgs = pod, token, args
		return "", "", nil
	}
	if err := forceBroadPATRotationDue(); err != nil {
		t.Fatalf("forceBroadPATRotationDue: %v", err)
	}
	if gotPod != rootOpenbaoPod || gotToken != "roottoken" {
		t.Errorf("bao exec pod=%q token=%q, want %q/roottoken", gotPod, gotToken, rootOpenbaoPod)
	}
	if want := []string{"kv", "patch", broadPATBaoPath, "rotated_at=0"}; strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("bao args = %v, want %v", gotArgs, want)
	}

	// An OpenBao failure surfaces (with stderr) — it must not be swallowed.
	baoExecFn = func(_, _, _ string, _ ...string) (string, string, error) {
		return "", "permission denied on secret/data/linode/broad-pat", fmt.Errorf("exit 2")
	}
	err := forceBroadPATRotationDue()
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected the OpenBao stderr surfaced, got %v", err)
	}
}

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
