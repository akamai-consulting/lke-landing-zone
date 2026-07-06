package main

import (
	"context"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/metrics"
)

// fakeGetter satisfies nodeGetter (GetJSON) with a canned object + status.
type fakeGetter struct {
	obj    map[string]any
	status int
}

func (f fakeGetter) GetJSON(context.Context, string) (map[string]any, int, error) {
	return f.obj, f.status, nil
}

func metricsDump(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if _, err := reg.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestSampleTokenInventoryExposesGauges(t *testing.T) {
	cm := map[string]any{
		"data": map[string]any{
			"inventory.json": `{"updated":1720000000,"region":"primary","tokens":[
			  {"provider":"github","name":"APL_VALUES_REPO_TOKEN","expiry":1725000000,"state":"ok"},
			  {"provider":"linode","name":"9:pat","expiry":0,"state":"breach"},
			  {"provider":"github","name":"warn-tok","expiry":1724000000,"state":"warn"}
			]}`,
		},
	}
	reg := metrics.NewRegistry()
	if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm, status: 200}, reg); err != nil {
		t.Fatal(err)
	}
	out := metricsDump(t, reg)
	for _, want := range []string{
		// timestamp values render in shortest-exact form (scientific notation is
		// fine — Prometheus parses it); assert the series exists, plus plain counts.
		`llz_token_inventory_updated_timestamp_seconds 1.72e+09`,
		`llz_token_inventory_tokens 3`,
		`llz_token_expiry_timestamp_seconds{provider="github",token="APL_VALUES_REPO_TOKEN"} `,
		`llz_token_audit_ok{provider="github",token="APL_VALUES_REPO_TOKEN"} 1`,
		`llz_token_audit_ok{provider="linode",token="9:pat"} 0`, // breach → 0
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
	// A breach/0-expiry token must NOT emit an expiry gauge (expiry unknown).
	if strings.Contains(out, `llz_token_expiry_timestamp_seconds{provider="linode",token="9:pat"}`) {
		t.Errorf("0-expiry token should not emit an expiry gauge:\n%s", out)
	}
}

// A 404 (the writer job hasn't run yet) is a clean no-op, not an error.
func TestSampleTokenInventoryAbsentIsNoOp(t *testing.T) {
	reg := metrics.NewRegistry()
	if err := sampleTokenInventory(context.Background(), fakeGetter{obj: nil, status: 404}, reg); err != nil {
		t.Fatalf("404 should be a no-op, got %v", err)
	}
	if strings.Contains(metricsDump(t, reg), "llz_token_") {
		t.Error("no token metrics should be published when the ConfigMap is absent")
	}
}

// A present-but-empty ConfigMap (no inventory.json) is also a clean no-op.
func TestSampleTokenInventoryEmptyIsNoOp(t *testing.T) {
	reg := metrics.NewRegistry()
	cm := map[string]any{"data": map[string]any{}}
	if err := sampleTokenInventory(context.Background(), fakeGetter{obj: cm, status: 200}, reg); err != nil {
		t.Fatalf("empty ConfigMap should be a no-op, got %v", err)
	}
}
