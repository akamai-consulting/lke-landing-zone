package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
)

// TestMain zeroes the probe retry delay for the whole package. Every probe now
// retries an unanswerable kubectl call, and the tests stub execOutput with
// errors — without this each stubbed failure would pay two real 3s sleeps.
func TestMain(m *testing.M) {
	probeDelay = 0
	os.Exit(m.Run())
}

func TestClassifyKubectlErr(t *testing.T) {
	absent := []error{
		errors.New("NotFound"),
		errors.New(`Error from server (NotFound): secrets "linode" not found`),
		errors.New("No resources found in kube-system namespace."),
		errors.New(`error: the server doesn't have a resource type "applications"`),
		&exec.ExitError{Stderr: []byte(`Error from server (NotFound): pods "platform-openbao-0" not found`)},
	}
	for _, err := range absent {
		if got := classifyKubectlErr(err); got != probeAbsent {
			t.Errorf("classifyKubectlErr(%q) = %v, want probeAbsent", err, got)
		}
	}

	// Everything else is NOT evidence of absence. These are the failures that
	// used to read as "the resource is gone".
	unknown := []error{
		errors.New("Unable to connect to the server: dial tcp 10.0.0.1:443: connect: connection refused"),
		errors.New("error: You must be logged in to the server (Unauthorized)"),
		errors.New(`Error from server (Forbidden): secrets "linode" is forbidden`),
		errors.New("context deadline exceeded"),
		errors.New("error: Timeout: request did not complete within 10s"),
		&exec.ExitError{Stderr: []byte("net/http: TLS handshake timeout")},
	}
	for _, err := range unknown {
		if got := classifyKubectlErr(err); got != probeUnknown {
			t.Errorf("classifyKubectlErr(%q) = %v, want probeUnknown", err, got)
		}
	}
}

func TestKExistsOKSeparatesAbsentFromUnreadable(t *testing.T) {
	// Present on the first try — no retries needed.
	calls := 0
	withExecOutput(t, func(string, ...string) ([]byte, error) { calls++; return nil, nil })
	if exists, answered := kExistsOK("get", "secret", "x"); !exists || !answered || calls != 1 {
		t.Errorf("present: got (%v,%v) in %d calls, want (true,true) in 1", exists, answered, calls)
	}

	// A genuine NotFound is an ANSWER — returned on the first attempt rather than
	// re-asking a question kubectl already settled.
	calls = 0
	withExecOutput(t, func(string, ...string) ([]byte, error) { calls++; return nil, errors.New("NotFound") })
	if exists, answered := kExistsOK("get", "secret", "x"); exists || !answered || calls != 1 {
		t.Errorf("absent: got (%v,%v) in %d calls, want (false,true) in 1", exists, answered, calls)
	}

	// A transient blip then success → present wins; a one-off error must not read
	// as absent (this is what secretPresentWithRetry did for one call site).
	calls = 0
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("connection refused")
		}
		return nil, nil
	})
	if exists, _ := kExistsOK("x"); !exists || calls != 2 {
		t.Errorf("blip-then-ok: got %v after %d calls, want true after 2", exists, calls)
	}

	// A blip that survives the retries is reported as unanswered, NOT as absent.
	calls = 0
	withExecOutput(t, func(string, ...string) ([]byte, error) { calls++; return nil, errors.New("connection refused") })
	exists, answered := kExistsOK("x")
	if exists || answered || calls != probeRetries {
		t.Errorf("unreadable: got (%v,%v) in %d calls, want (false,false) in %d", exists, answered, calls, probeRetries)
	}
	// kExists still collapses to "absent" — safe only where absence hard-fails.
	if kExists("x") {
		t.Error("kExists should read an unanswerable probe as absent")
	}
}

func TestKItemsOKAndJSONPathOKReportUnreadable(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("i/o timeout") })
	if items, ok := kItemsOK("get", "pods"); ok || items != nil {
		t.Errorf("kItemsOK on unreadable: got (%v,%v), want (nil,false)", items, ok)
	}
	if val, ok := kJSONPathOK("get", "sts", "x", "-o", "jsonpath={.spec.replicas}"); ok || val != "" {
		t.Errorf("kJSONPathOK on unreadable: got (%q,%v), want (\"\",false)", val, ok)
	}

	// A NotFound IS an answer: empty, but true — the caller can distinguish
	// "no such resource" from "could not ask".
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("NotFound") })
	if _, ok := kJSONPathOK("get", "sts", "x"); !ok {
		t.Error("kJSONPathOK on NotFound: want answered=true")
	}

	// A well-formed exit with an unparseable body is not an answer either.
	withExecOutput(t, func(string, ...string) ([]byte, error) { return []byte("not json"), nil })
	if _, ok := kItemsOK("get", "pods"); ok {
		t.Error("kItemsOK on unparseable body: want answered=false")
	}
}

// TestSectionsRefuseEmptyCorpus is the cluster-probe half of
// TestWaveHealthGuardFailsOnEmptyCorpus: a section whose list call failed must
// not iterate zero items and report the same green as a full pass.
func TestSectionsRefuseEmptyCorpus(t *testing.T) {
	withExecOutput(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("Unable to connect to the server: connection refused")
	})
	r := &health.Report{}
	checkAPIServices(r)
	checkWebhooks(r)
	if len(r.Pending) == 0 {
		t.Fatal("unreadable cluster: sections recorded nothing — an empty corpus passed as green")
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
