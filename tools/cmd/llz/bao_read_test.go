package main

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
		if got := classifyBaoRead(s); got != baoReadAbsent {
			t.Errorf("classifyBaoRead(%q) = %v, want absent", s, got)
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
		if got := classifyBaoRead(s); got != baoReadUnknown {
			t.Errorf("classifyBaoRead(%q) = %v, want unknown", s, got)
		}
	}

	// A seal complaint that also happens to carry an absence phrase must not be
	// read as absence — denials are checked first.
	if got := classifyBaoRead("Vault is sealed; no value found"); got != baoReadUnknown {
		t.Errorf("sealed+absence phrasing = %v, want unknown", got)
	}

	// Unrecognized text is not decided here; the liveness probe decides.
	if got := classifyBaoRead("something entirely new"); got != baoReadIndeterminate {
		t.Errorf("unknown phrasing = %v, want indeterminate", got)
	}
}

// withBaoRead stubs the exec seam for a KV read, returning the given stderr/error,
// and pins whether the pod answers its own status probe.
func withBaoRead(t *testing.T, stderr string, podHealthy bool) {
	t.Helper()
	prev := baoExecFn
	baoExecFn = func(_, _, _ string, args ...string) (string, string, error) {
		if args[0] == "status" {
			if !podHealthy {
				return "", "connection refused", errors.New("exit 2")
			}
			return `{"initialized":true,"sealed":false}`, "", nil
		}
		return "", stderr, errors.New("exit 2")
	}
	t.Cleanup(func() { baoExecFn = prev })
}

// Bao's absence phrasing varies by version, so an unrecognized stderr must not
// resolve by guessing: ask the pod. A healthy pod that refused the read is
// answering about the path; a pod that will not answer tells us nothing.
func TestBaoKVGetFieldOKUsesLivenessForUnrecognizedErrors(t *testing.T) {
	withBaoRead(t, "some future bao phrasing", true)
	if _, v := baoKVGetFieldOK("secret/x", "y"); v != baoReadAbsent {
		t.Errorf("unrecognized error + healthy pod = %v, want absent (a cold bootstrap must still seed)", v)
	}

	withBaoRead(t, "some future bao phrasing", false)
	if _, v := baoKVGetFieldOK("secret/x", "y"); v != baoReadUnknown {
		t.Errorf("unrecognized error + unreachable pod = %v, want unknown", v)
	}

	// An explicit seal never consults liveness — sealed IS the answer.
	withBaoRead(t, "Vault is sealed", true)
	if _, v := baoKVGetFieldOK("secret/x", "y"); v != baoReadUnknown {
		t.Errorf("sealed = %v, want unknown even when the status probe answers", v)
	}
}

// The seeder must not overwrite a path it could not read. This is the bug:
// a sealed pod read "" and the guard let fresh crypto/rand bytes land on a live
// credential.
func TestBaoSeedRefusesToWriteOnUnreadablePath(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	withBaoRead(t, "Vault is sealed", false)

	wrote := false
	prevPut := baoKVPutFn
	baoKVPutFn = func(string, map[string]string) error { wrote = true; return nil }
	t.Cleanup(func() { baoKVPutFn = prevPut })

	err := runCIBaoSeed(baoSeedOpts{
		path:          "secret/grafana/admin",
		fieldSpecs:    []string{"password=gen:hex:16"},
		skipIfPresent: "password",
		onMissing:     "error",
	})
	if err == nil {
		t.Fatal("an unreadable path must fail the seed, not silently overwrite it")
	}
	if wrote {
		t.Fatal("a credential was overwritten on the strength of a failed read")
	}
	if !strings.Contains(err.Error(), "NOT evidence") {
		t.Errorf("the error must say what it did not conclude: %v", err)
	}
}

// The mint paths create real cloud resources on the same "" — a Linode
// object-storage key and an in-cluster PAT.
func TestMintPathsRefuseOnUnreadablePath(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("LINODE_API_TOKEN", "broad")
	withBaoRead(t, "connection refused", false)

	if err := runCIMintBootstrapPAT("primary"); err == nil ||
		!strings.Contains(err.Error(), "NOT evidence") {
		t.Errorf("mint-bootstrap-pat on an unreadable path: err = %v, want a fail-closed refusal", err)
	}
}
