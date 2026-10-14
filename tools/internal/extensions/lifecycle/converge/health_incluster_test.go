package converge

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
)

func TestConvergenceExit(t *testing.T) {
	failed := health.Report{Failed: []string{"x hard-failed"}}
	pending := health.Report{Pending: []string{"y in-progress"}}
	ok := health.Report{}
	cases := []struct {
		name        string
		r           health.Report
		crd, failOn bool
		want        int
	}{
		{"converged-gate", ok, true, true, 0},
		{"failed-gate", failed, true, true, 1},
		{"pending-gate", pending, true, true, 2},
		{"pre-bootstrap-gate", ok, false, true, 2}, // CRD absent = in-progress
		{"failed-report-only", failed, true, false, 0},
		{"pending-report-only", pending, true, false, 0},
		{"pre-bootstrap-report-only", ok, false, false, 0},
	}
	for _, c := range cases {
		if got := ConvergenceExit(c.r, c.crd, c.failOn); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
