package baoread

// Every test here describes the same mistake: a read that failed, believed as a
// statement about the path, followed by a write that destroys a live credential.

import (
	"errors"
	"strings"
	"testing"
)

func TestClassifyBaoRead(t *testing.T) {
	absent := []string{
		"No value found at secret/data/linode/api-token",
		"Field 'token' not present in secret",
		"No secret found at secret/harbor/robot",
	}
	for _, s := range absent {
		if got := Classify(s); got != Absent {
			t.Errorf("Classify(%q) = %v, want absent", s, got)
		}
	}

	// These are the ones that used to read as "the path is empty".
	denied := []string{
		"Error making API request... Code: 503. Errors: * Vault is sealed",
		"Code: 403. Errors: * permission denied",
		"missing client token",
		"Get \"https://127.0.0.1:8200\": dial tcp: connection refused",
		"error dialing backend: No agent available",
		"net/http: TLS handshake timeout",
	}
	for _, s := range denied {
		if got := Classify(s); got != Unknown {
			t.Errorf("Classify(%q) = %v, want unknown", s, got)
		}
	}

	// A seal complaint that also happens to carry an absence phrase must not be
	// read as absence — denials are checked first.
	if got := Classify("Vault is sealed; no value found"); got != Unknown {
		t.Errorf("sealed+absence phrasing = %v, want unknown", got)
	}

	// Unrecognized text is not decided here; the liveness probe decides.
	if got := Classify("something entirely new"); got != Indeterminate {
		t.Errorf("unknown phrasing = %v, want indeterminate", got)
	}
}

// withBaoRead stubs the exec seam for a KV read, returning the given stderr/error,
// and pins whether the pod answers its own status probe.
func withBaoRead(t *testing.T, stderr string, podHealthy bool) {
	t.Helper()
	prevExec, prevStatus := Exec, PodStatusUnsealed
	Exec = func(_ string, args ...string) (string, string, error) {
		if args[0] == "status" {
			if !podHealthy {
				return "", "connection refused", errors.New("exit 2")
			}
			return `{"initialized":true,"sealed":false}`, "", nil
		}
		return "", stderr, errors.New("exit 2")
	}
	// BOTH seams, not just Exec. The liveness check is the tiebreaker this
	// package's whole discipline rests on, and leaving it at its default (false)
	// would make every unrecognised stderr resolve Unknown regardless of what the
	// fake pod said — the test would pass while asserting nothing.
	PodStatusUnsealed = func(out string) bool {
		return strings.Contains(out, `"sealed":false`)
	}
	t.Cleanup(func() { Exec, PodStatusUnsealed = prevExec, prevStatus })
}

// Bao's absence phrasing varies by version, so an unrecognized stderr must not
// resolve by guessing: ask the pod. A healthy pod that refused the read is
// answering about the path; a pod that will not answer tells us nothing.
func TestBaoKVGetFieldOKUsesLivenessForUnrecognizedErrors(t *testing.T) {
	withBaoRead(t, "some future bao phrasing", true)
	if _, v := KVGetFieldOK("secret/x", "y"); v != Absent {
		t.Errorf("unrecognized error + healthy pod = %v, want absent (a cold bootstrap must still seed)", v)
	}

	withBaoRead(t, "some future bao phrasing", false)
	if _, v := KVGetFieldOK("secret/x", "y"); v != Unknown {
		t.Errorf("unrecognized error + unreachable pod = %v, want unknown", v)
	}

	// An explicit seal never consults liveness — sealed IS the answer.
	withBaoRead(t, "Vault is sealed", true)
	if _, v := KVGetFieldOK("secret/x", "y"); v != Unknown {
		t.Errorf("sealed = %v, want unknown even when the status probe answers", v)
	}
}
