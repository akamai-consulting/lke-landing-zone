package preflight

import "testing"

func TestVPCQuotaExceeded(t *testing.T) {
	// At the cap, the +1 for this apply tips it over.
	if !VPCQuotaExceeded(10, 1, 10) {
		t.Error("10 existing + 1 > 10 limit should be exceeded")
	}
	// One below the cap: total+1 == limit, not over.
	if VPCQuotaExceeded(9, 1, 10) {
		t.Error("9 + 1 == 10 limit should NOT be exceeded")
	}
	// Unset limit never trips.
	if VPCQuotaExceeded(100, 1, 0) {
		t.Error("unset limit (0) must be report-only")
	}
}

func TestVCPUQuotaExceeded(t *testing.T) {
	if !VCPUQuotaExceeded(30, 8, 36) {
		t.Error("30 used + 8 > 36 limit should be exceeded")
	}
	if VCPUQuotaExceeded(28, 8, 36) {
		t.Error("28 + 8 == 36 limit should NOT be exceeded")
	}
	if VCPUQuotaExceeded(1000, 8, 0) {
		t.Error("unset limit must be report-only")
	}
}

func TestPoolVCPU(t *testing.T) {
	if got := PoolVCPU(4, 3); got != 12 {
		t.Errorf("PoolVCPU(4,3) = %d, want 12", got)
	}
	if got := PoolVCPU(0, 3); got != 0 {
		t.Errorf("PoolVCPU(0,3) = %d, want 0 (unknown type)", got)
	}
}

func TestSameLabelExcess(t *testing.T) {
	if SameLabelExcess(0) || SameLabelExcess(1) {
		t.Error("0 or 1 live clusters with the label is healthy")
	}
	if !SameLabelExcess(2) {
		t.Error("2+ live clusters with the same label is an excess")
	}
}

func TestOrphansExceedThreshold(t *testing.T) {
	if OrphansExceedThreshold(5, 5) {
		t.Error("orphans == threshold should not exceed")
	}
	if !OrphansExceedThreshold(6, 5) {
		t.Error("orphans > threshold should exceed")
	}
	if !OrphansExceedThreshold(1, 0) {
		t.Error("1 orphan with threshold 0 should exceed")
	}
}
