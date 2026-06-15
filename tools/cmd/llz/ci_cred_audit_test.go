package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
)

// fakeLister implements credLister with canned data — the seam that lets the
// PAT policy branches be tested without touching the real Linode API.
type fakeLister struct {
	tokens []map[string]any
	keys   []map[string]any
	err    error
}

func (f fakeLister) ListProfileTokens(context.Context) ([]map[string]any, error) {
	return f.tokens, f.err
}
func (f fakeLister) ListObjectStorageKeys(context.Context) ([]map[string]any, error) {
	return f.keys, f.err
}

// ts renders a Linode-API timestamp offsetDays from now (negative = past).
func ts(offsetDays int64) string {
	return linode.FmtLinodeTS(time.Now().Unix() + offsetDays*linode.DaySecs)
}

func TestUnlabelled(t *testing.T) {
	if got := unlabelled(""); got != "<unlabelled>" {
		t.Errorf("unlabelled(\"\") = %q, want <unlabelled>", got)
	}
	if got := unlabelled("loki-prod"); got != "loki-prod" {
		t.Errorf("unlabelled(label) = %q, want loki-prod", got)
	}
}

func TestCredAuditPassesWhenAllPATsCompliant(t *testing.T) {
	f := fakeLister{tokens: []map[string]any{
		{"id": json.Number("1"), "label": "tf", "created": ts(-10), "expiry": ts(60)},
	}}
	rec, violated, err := credAudit(context.Background(), f, credAuditOpts{maxPATDays: 90, warnDays: 14})
	if err != nil || violated {
		t.Fatalf("credAudit = (violated=%v, %v), want (false, nil)", violated, err)
	}
	if rec["result"] != "PASS" {
		t.Errorf("result = %v, want PASS", rec["result"])
	}
}

func TestCredAuditFailsOnPolicyViolations(t *testing.T) {
	cases := []struct {
		name  string
		token map[string]any
	}{
		{"no_expiry", map[string]any{"id": json.Number("1"), "label": "a", "created": ts(-1)}},
		{"expired", map[string]any{"id": json.Number("2"), "label": "b", "created": ts(-100), "expiry": ts(-1)}},
		{"lifetime_exceeds", map[string]any{"id": json.Number("3"), "label": "c", "created": ts(0), "expiry": ts(100)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeLister{tokens: []map[string]any{tc.token}}
			rec, violated, err := credAudit(context.Background(), f, credAuditOpts{maxPATDays: 90, warnDays: 14})
			if err != nil || !violated {
				t.Fatalf("credAudit = (violated=%v, %v), want (true, nil)", violated, err)
			}
			if rec["result"] != "FAIL" {
				t.Errorf("result = %v, want FAIL", rec["result"])
			}
		})
	}
}

func TestCredAuditNearExpiryWarnsButPassesUnlessStrict(t *testing.T) {
	near := map[string]any{"id": json.Number("4"), "label": "d", "created": ts(-80), "expiry": ts(5)}

	rec, violated, err := credAudit(context.Background(),
		fakeLister{tokens: []map[string]any{near}}, credAuditOpts{maxPATDays: 90, warnDays: 14})
	if err != nil || violated {
		t.Fatalf("non-strict = (violated=%v, %v), want (false, nil)", violated, err)
	}
	if rec["result"] != "PASS_WITH_WARNINGS" {
		t.Errorf("result = %v, want PASS_WITH_WARNINGS", rec["result"])
	}

	rec, violated, err = credAudit(context.Background(),
		fakeLister{tokens: []map[string]any{near}}, credAuditOpts{maxPATDays: 90, warnDays: 14, strict: true})
	if err != nil || !violated {
		t.Fatalf("strict = (violated=%v, %v), want (true, nil)", violated, err)
	}
	if rec["result"] != "FAIL" {
		t.Errorf("strict result = %v, want FAIL", rec["result"])
	}
}

func TestCredAuditInventoriesObjectStorageKeys(t *testing.T) {
	f := fakeLister{
		tokens: []map[string]any{{"id": json.Number("1"), "label": "tf", "created": ts(-1), "expiry": ts(60)}},
		keys: []map[string]any{
			{"id": json.Number("10"), "label": "loki-prod", "limited": true, "bucket_access": []any{map[string]any{}}},
			{"id": json.Number("11"), "label": "tf-state"},
		},
	}
	rec, _, err := credAudit(context.Background(), f, credAuditOpts{maxPATDays: 90, warnDays: 14})
	if err != nil {
		t.Fatal(err)
	}
	keys, ok := rec["obj_keys"].([]any)
	if !ok || len(keys) != 2 {
		t.Fatalf("obj_keys = %v, want 2 entries", rec["obj_keys"])
	}
	if keys[0].(map[string]any)["is_loki_key"] != true {
		t.Error("loki-prod key should be flagged is_loki_key")
	}
}

func TestCredAuditReturnsAPIErrors(t *testing.T) {
	if _, _, err := credAudit(context.Background(), fakeLister{err: io.ErrUnexpectedEOF},
		credAuditOpts{maxPATDays: 90, warnDays: 14}); err == nil {
		t.Fatal("API error must surface")
	}
}

// runCICredAudit wrapper behavior: skip-when-unbootstrapped, summary, verdict.
func TestRunCICredAudit(t *testing.T) {
	sum := filepath.Join(t.TempDir(), "sum")
	t.Setenv("GITHUB_STEP_SUMMARY", sum)
	t.Setenv("REGION", "primary")

	// No token in the environment → graceful skip with a summary note.
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("LINODE_API_TOKEN", "")
	if err := runCICredAudit(credAuditOpts{maxPATDays: 90, warnDays: 14}); err != nil {
		t.Fatalf("unbootstrapped env must skip cleanly: %v", err)
	}
	if b, _ := os.ReadFile(sum); !strings.Contains(string(b), "env not bootstrapped") {
		t.Errorf("summary missing the skip note:\n%s", b)
	}

	// Violation → error (fails the step) + CRITICAL summary line.
	t.Setenv("LINODE_TOKEN", "tok")
	prev := newCredAuditClient
	newCredAuditClient = func(string) credLister {
		return fakeLister{tokens: []map[string]any{{"id": json.Number("1"), "label": "a"}}}
	}
	t.Cleanup(func() { newCredAuditClient = prev })
	_ = captureStdout(t, func() {
		if err := runCICredAudit(credAuditOpts{maxPATDays: 90, warnDays: 14}); err == nil {
			t.Error("policy breach must fail the step")
		}
	})
	if b, _ := os.ReadFile(sum); !strings.Contains(string(b), "CRITICAL") {
		t.Errorf("summary missing the CRITICAL verdict:\n%s", b)
	}

	// Compliant → nil, audit record on stdout.
	newCredAuditClient = func(string) credLister {
		return fakeLister{tokens: []map[string]any{
			{"id": json.Number("1"), "label": "tf", "created": ts(-10), "expiry": ts(60)},
		}}
	}
	out := captureStdout(t, func() {
		if err := runCICredAudit(credAuditOpts{maxPATDays: 90, warnDays: 14}); err != nil {
			t.Errorf("compliant audit must pass: %v", err)
		}
	})
	if !strings.Contains(out, `"event":"linode-cred-audit"`) {
		t.Errorf("stdout missing the audit record:\n%s", out)
	}
}
