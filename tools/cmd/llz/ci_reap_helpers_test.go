package main

// Unit tests for the pure reaping/preflight helpers in ci.go and
// ci_preflight.go: the count/attribution logic that decides whether a Volume or
// NodeBalancer belongs to a cluster, seamed behind small list interfaces so it
// runs without the live Linode API.

import (
	"context"
	"errors"
	"testing"
)

// fakeListClient serves canned Volume / NodeBalancer lists, optionally failing.
type fakeListClient struct {
	volumes []map[string]any
	nbs     []map[string]any
	err     error
}

func (f *fakeListClient) ListVolumes(context.Context) ([]map[string]any, error) {
	return f.volumes, f.err
}
func (f *fakeListClient) ListNodeBalancers(context.Context) ([]map[string]any, error) {
	return f.nbs, f.err
}

func TestCountVolumesPresent(t *testing.T) {
	client := &fakeListClient{volumes: []map[string]any{
		{"id": float64(1)},
		{"id": float64(2)},
		{"id": float64(3)},
	}}

	// Only ids 1 and 3 are tracked; id 2 is ignored, unknown id 9 matches nothing.
	n, err := countVolumesPresent(context.Background(), client, "1 3 9")
	if err != nil {
		t.Fatalf("countVolumesPresent: %v", err)
	}
	if n != 2 {
		t.Errorf("present = %d, want 2", n)
	}

	// A list error surfaces as the -1 sentinel.
	bad := &fakeListClient{err: errors.New("boom")}
	if n, err := countVolumesPresent(context.Background(), bad, "1"); err == nil || n != -1 {
		t.Errorf("on list error got (%d, %v), want (-1, err)", n, err)
	}
}

func TestItoaOrUnknown(t *testing.T) {
	if got := itoaOrUnknown(-1); got != "" {
		t.Errorf("itoaOrUnknown(-1) = %q, want \"\"", got)
	}
	if got := itoaOrUnknown(0); got != "0" {
		t.Errorf("itoaOrUnknown(0) = %q, want \"0\"", got)
	}
	if got := itoaOrUnknown(42); got != "42" {
		t.Errorf("itoaOrUnknown(42) = %q, want \"42\"", got)
	}
}

func TestNBBelongsToCluster(t *testing.T) {
	// Reliable owner link via lke_cluster.id.
	viaLink := map[string]any{"lke_cluster": map[string]any{"id": float64(555)}}
	if !nbBelongsToCluster(viaLink, "555") {
		t.Error("expected match via lke_cluster.id")
	}
	// Fallback via the CCM `lke<id>` tag.
	viaTag := map[string]any{"tags": []any{"kubernetes", "lke777"}}
	if !nbBelongsToCluster(viaTag, "777") {
		t.Error("expected match via lke tag")
	}
	// LKE-E CCM NodeBalancer tagged only `kubernetes`, different owner — no match.
	other := map[string]any{
		"lke_cluster": map[string]any{"id": float64(111)},
		"tags":        []any{"kubernetes"},
	}
	if nbBelongsToCluster(other, "555") {
		t.Error("did not expect match for a different cluster")
	}
}

func TestCountClusterNodeBalancersPresent(t *testing.T) {
	client := &fakeListClient{nbs: []map[string]any{
		{"lke_cluster": map[string]any{"id": float64(42)}}, // match via link
		{"tags": []any{"lke42"}},                           // match via tag
		{"lke_cluster": map[string]any{"id": float64(99)}}, // other cluster
		{"tags": []any{"kubernetes"}},                      // unattributable
	}}

	n, err := countClusterNodeBalancersPresent(context.Background(), client, "42")
	if err != nil {
		t.Fatalf("countClusterNodeBalancersPresent: %v", err)
	}
	if n != 2 {
		t.Errorf("present = %d, want 2", n)
	}

	bad := &fakeListClient{err: errors.New("boom")}
	if n, err := countClusterNodeBalancersPresent(context.Background(), bad, "42"); err == nil || n != -1 {
		t.Errorf("on list error got (%d, %v), want (-1, err)", n, err)
	}
}

func TestOrQ(t *testing.T) {
	if got := orQ("v", true); got != "?" {
		t.Errorf("orQ(unknown) = %q, want \"?\"", got)
	}
	if got := orQ("v", false); got != "v" {
		t.Errorf("orQ(known) = %q, want \"v\"", got)
	}
}
