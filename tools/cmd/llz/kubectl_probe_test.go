package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// TestMain zeroes the probe retry delay for the whole package. Every probe now
// retries an unanswerable kubectl call, and the tests stub execOutput with
// errors — without this each stubbed failure would pay two real 3s sleeps.
// It also catches renderRootsFn's `<self> render <env> --tfvars-only` shell-out:
// under `go test` os.Executable() is THIS binary, so without the guard every
// shell-out re-runs the whole suite (recursively) and the run hangs instead of
// failing. See renderReexecChild in fetchkubeconfig_state_deadline_test.go.
func TestMain(m *testing.M) {
	kubectlprobe.Delay = 0
	if renderReexecChild() {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestSectionsRefuseEmptyCorpus is the cluster-probe half of
// TestWaveHealthGuardFailsOnEmptyCorpus: a section whose list call failed must
// not iterate zero items and report the same color.Green as a full pass.
func TestSectionsRefuseEmptyCorpus(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("Unable to connect to the server: connection refused")
	})
	r := &health.Report{}
	checkAPIServices(r)
	checkWebhooks(r)
	if len(r.Pending) == 0 {
		t.Fatal("unreadable cluster: sections recorded nothing — an empty corpus passed as color.Green")
	}
	for _, p := range r.Pending {
		if !strings.Contains(p, "could not list") {
			t.Errorf("unexpected pending finding: %q", p)
		}
	}

	// A cluster that answers with a genuinely empty list is still a clean pass.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte(`{"items":[]}`), nil })
	r = &health.Report{}
	checkAPIServices(r)
	checkWebhooks(r)
	if len(r.Pending) != 0 || len(r.Failed) != 0 {
		t.Errorf("empty-but-answered cluster should pass: pending=%v failed=%v", r.Pending, r.Failed)
	}
}

func TestCheckFirewallBootstrapDoesNotSkipOnUnreadable(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("Unable to connect to the server: connection refused")
	})
	r := &health.Report{}
	checkFirewallBootstrap(r)
	if len(r.Pending) != 1 || !strings.Contains(r.Pending[0], "could not read") {
		t.Fatalf("unreadable firewall probes should be inconclusive, got pending=%v", r.Pending)
	}

	// Genuinely absent (component disabled) still skips the section with an OK.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("NotFound") })
	r = &health.Report{}
	checkFirewallBootstrap(r)
	if len(r.Pending) != 0 || len(r.Failed) != 0 {
		t.Errorf("cidrFirewall disabled should skip clean: pending=%v failed=%v", r.Pending, r.Failed)
	}
}

// TestCheckFirewallBootstrapSelfDiscoveryNoController pins the depExists-gating:
// when the in-cluster self-discovery has written the ConfigMap but the private
// controller Deployment is absent (public adopters / e2e), the kube-system/linode
// token Secret is consumed by nothing, so its absence must NOT hard-fail.
func TestCheckFirewallBootstrapSelfDiscoveryNoController(t *testing.T) {
	withExecOutput(t, func(_ string, args ...string) ([]byte, error) {
		a := strings.Join(args, " ")
		switch {
		case strings.Contains(a, "deployment"):
			return nil, errors.New("NotFound") // controller Deployment absent
		case strings.Contains(a, "configmap"):
			return []byte("llz-linode-cidr-firewall-config"), nil // self-discovery ConfigMap present
		case strings.Contains(a, "secret"):
			return nil, errors.New("NotFound") // token Secret never seeded
		default:
			return nil, errors.New("NotFound")
		}
	})
	r := &health.Report{}
	checkFirewallBootstrap(r)
	if len(r.Failed) != 0 {
		t.Errorf("self-discovery ConfigMap without the controller Deployment must not hard-fail on the missing token: failed=%v", r.Failed)
	}
	if len(r.Pending) != 0 {
		t.Errorf("controller-absent cluster is a clean pass, not pending: pending=%v", r.Pending)
	}
}
