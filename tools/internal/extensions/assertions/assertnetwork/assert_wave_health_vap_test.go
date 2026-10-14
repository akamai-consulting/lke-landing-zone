package assertnetwork

import (
	"errors"
	"strings"
	"testing"
)

func TestClassifyWaveHealthCanary(t *testing.T) {
	denial := `Error from server (Invalid): error when creating "STDIN": admission webhook denied: ` +
		"ValidatingAdmissionPolicy 'llz-wave-health-guard' with binding 'llz-wave-health-guard' denied request: " +
		"wave-health-guard (admission): apps/Deployment at sync-wave -5 is not a vetted health-safe kind."

	tests := []struct {
		name   string
		out    string
		err    error
		wantOK bool
		// wantMsg is a substring the verdict must explain itself with.
		wantMsg string
	}{
		{
			name:    "denied by the guard is the pass case",
			out:     denial,
			err:     errors.New("exit status 1"),
			wantOK:  true,
			wantMsg: "bound and enforcing",
		},
		{
			// The regression this verb exists to catch: policy absent, binding
			// missing, or the binding downgraded to Audit/Warn.
			name:    "admitted means the guard is not enforcing",
			out:     "deployment.apps/llz-wave-health-canary created (server dry run)",
			err:     nil,
			wantOK:  false,
			wantMsg: "ADMITTED",
		},
		{
			// A denial from PSS/Kyverno/quota must NOT be read as proof the
			// wave-health guard works — we never observed it run.
			name:    "denied by an unrelated policy is inconclusive, not a pass",
			out:     "Error from server: admission webhook \"validate.kyverno.svc\" denied the request: image not signed",
			err:     errors.New("exit status 1"),
			wantOK:  false,
			wantMsg: "NOT by llz-wave-health-guard",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, msg := classifyWaveHealthCanary(tt.out, tt.err)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v (msg: %s)", ok, tt.wantOK, msg)
			}
			if !strings.Contains(msg, tt.wantMsg) {
				t.Errorf("msg = %q, want it to contain %q", msg, tt.wantMsg)
			}
		})
	}
}

func TestCIAssertWaveHealthVAPCmdWiring(t *testing.T) {
	c := WaveHealthVAPCmd()
	if c.Use != "assert-wave-health-vap" {
		t.Errorf("Use = %q, want assert-wave-health-vap", c.Use)
	}
	if err := c.Args(c, nil); err != nil {
		t.Errorf("Args(nil) = %v, want nil", err)
	}
}
