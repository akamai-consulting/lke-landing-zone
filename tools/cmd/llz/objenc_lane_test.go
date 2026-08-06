package main

// objenc_lane_test.go — the obj-encryption gate must be WIRED IN.
//
// THIS COUPLING TEST STAYS ON THIS SIDE because assertSuiteLanes does: the catalog
// names ci_assert_suite.go as the core-owned required-assertion set, and ADR 0014's
// corollary keeps it there. The gate itself moved to internal/objenc; the claim
// that anything runs it is a fact about the battery.
//
// The lane list is the one place a new gate can be declared and never actually
// run, which is why this assertion exists at all.

import (
	"strings"
	"testing"
)

// The lane must be in the battery, GATING, and it must carry the harbor-bucket —
// without which the CA-chain check silently does not run. The lane list is the one
// place a new gate can be declared and never actually run.
func TestObjEncryptionLaneIsRegisteredAndGating(t *testing.T) {
	lanes := assertSuiteLanes("e2e")
	var found *suiteLane
	for i := range lanes {
		if lanes[i].Name == "obj-encryption" {
			found = &lanes[i]
		}
	}
	if found == nil {
		t.Fatal("obj-encryption is not in assertSuiteLanes — nothing would ever run the gate")
	}
	if !found.Gating {
		t.Error("the lane must GATE: a report-only encryption check is a check nobody acts on")
	}
	flat := strings.Join(found.Steps[0], " ")
	if !strings.Contains(flat, "assert-obj-encryption") || !strings.Contains(flat, "--region") {
		t.Errorf("lane step must invoke the gate with a region: %q", flat)
	}
	// It must carry NOTHING else. Endpoint and bucket names are derived from the
	// spec; the earlier revision passed them from three env vars that no workflow
	// exported, so the lane would have failed on a missing flag rather than on
	// encryption — a gate misconfigured into always-color.Red teaches people to ignore it.
	for _, invented := range []string{"OBJ_ENDPOINT_HOST", "LOKI_CHUNKS_BUCKET", "HARBOR_REGISTRY_BUCKET"} {
		if strings.Contains(flat, invented) {
			t.Errorf("lane still reads %s, which nothing sets: %q", invented, flat)
		}
	}
}
