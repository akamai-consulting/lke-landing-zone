package main

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

func TestWaveHealthCanaryIsAKindTheGuardMustReject(t *testing.T) {
	// The canary only proves anything if the guard would genuinely deny it: a
	// health-checked kind ABSENT from the allowlist, at a NEGATIVE sync-wave, and
	// not an Argo hook (hooks are exempt via the VAP's not-argo-hook
	// matchCondition). Guard against someone "fixing" the canary into an
	// allowlisted kind, which would make this assert silently unfalsifiable.
	if !strings.Contains(waveHealthCanaryManifest, `argocd.argoproj.io/sync-wave: "-5"`) {
		t.Error("canary must carry a NEGATIVE sync-wave or the VAP's matchConditions skip it")
	}
	if strings.Contains(waveHealthCanaryManifest, "argocd.argoproj.io/hook") {
		t.Error("canary must not be an Argo hook — the VAP exempts hooks")
	}
	if _, allowlisted := waveHealthAllowedKinds["apps/Deployment"]; allowlisted {
		t.Error("apps/Deployment became allowlisted — the canary would now be ADMITTED and this assert would fail on a healthy cluster; pick another unvetted health-checked kind")
	}
}

func TestCIAssertWaveHealthVAPCmdWiring(t *testing.T) {
	c := ciAssertWaveHealthVAPCmd()
	if c.Use != "assert-wave-health-vap" {
		t.Errorf("Use = %q, want assert-wave-health-vap", c.Use)
	}
	if err := c.Args(c, nil); err != nil {
		t.Errorf("Args(nil) = %v, want nil", err)
	}
}
