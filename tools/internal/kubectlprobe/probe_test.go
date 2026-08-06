package kubectlprobe

// probe_test.go — the three tests that are ABOUT the probes, moved with them.
//
// The other three in the origin file (TestSectionsRefuseEmptyCorpus and the two
// firewall-bootstrap cases) stayed in package main: they assert what a SECTION
// does when a probe comes back unanswered, which is main's behaviour, not this
// package's. Tests travel with the file by default and that is usually wrong —
// the question is which package's claim the test is making.
//
// TestMain also stayed, and had to: it is package main's re-exec guard.

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// withExec swaps the shell-out seam for one test. The package-main original
// (withExecOutput) swaps two seams because main holds its own; here there is only
// this one.
func withExec(t *testing.T, fn func(name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := Exec
	Exec = fn
	t.Cleanup(func() { Exec = orig })
}

func TestMain(m *testing.M) {
	Delay = 0
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
		if got := ClassifyErr(err); got != Absent {
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
		if got := ClassifyErr(err); got != Unknown {
			t.Errorf("classifyKubectlErr(%q) = %v, want probeUnknown", err, got)
		}
	}
}

func TestKExistsOKSeparatesAbsentFromUnreadable(t *testing.T) {
	// Present on the first try — no retries needed.
	calls := 0
	withExec(t, func(string, ...string) ([]byte, error) { calls++; return nil, nil })
	if exists, answered := ExistsOK("get", "secret", "x"); !exists || !answered || calls != 1 {
		t.Errorf("present: got (%v,%v) in %d calls, want (true,true) in 1", exists, answered, calls)
	}

	// A genuine NotFound is an ANSWER — returned on the first attempt rather than
	// re-asking a question kubectl already settled.
	calls = 0
	withExec(t, func(string, ...string) ([]byte, error) { calls++; return nil, errors.New("NotFound") })
	if exists, answered := ExistsOK("get", "secret", "x"); exists || !answered || calls != 1 {
		t.Errorf("absent: got (%v,%v) in %d calls, want (false,true) in 1", exists, answered, calls)
	}

	// A transient blip then success → present wins; a one-off error must not read
	// as absent (this is what secretPresentWithRetry did for one call site).
	calls = 0
	withExec(t, func(string, ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("connection refused")
		}
		return nil, nil
	})
	if exists, _ := ExistsOK("x"); !exists || calls != 2 {
		t.Errorf("blip-then-ok: got %v after %d calls, want true after 2", exists, calls)
	}

	// A blip that survives the retries is reported as unanswered, NOT as absent.
	calls = 0
	withExec(t, func(string, ...string) ([]byte, error) { calls++; return nil, errors.New("connection refused") })
	exists, answered := ExistsOK("x")
	if exists || answered || calls != Retries {
		t.Errorf("unreadable: got (%v,%v) in %d calls, want (false,false) in %d", exists, answered, calls, Retries)
	}
	// kExists still collapses to "absent" — safe only where absence hard-fails.
	if Exists("x") {
		t.Error("kExists should read an unanswerable probe as absent")
	}
}

func TestKItemsOKAndJSONPathOKReportUnreadable(t *testing.T) {
	withExec(t, func(string, ...string) ([]byte, error) { return nil, errors.New("i/o timeout") })
	if items, ok := ItemsOK("get", "pods"); ok || items != nil {
		t.Errorf("kItemsOK on unreadable: got (%v,%v), want (nil,false)", items, ok)
	}
	if val, ok := JSONPathOK("get", "sts", "x", "-o", "jsonpath={.spec.replicas}"); ok || val != "" {
		t.Errorf("kJSONPathOK on unreadable: got (%q,%v), want (\"\",false)", val, ok)
	}

	// A NotFound IS an answer: empty, but true — the caller can distinguish
	// "no such resource" from "could not ask".
	withExec(t, func(string, ...string) ([]byte, error) { return nil, errors.New("NotFound") })
	if _, ok := JSONPathOK("get", "sts", "x"); !ok {
		t.Error("kJSONPathOK on NotFound: want answered=true")
	}

	// A well-formed exit with an unparseable body is not an answer either.
	withExec(t, func(string, ...string) ([]byte, error) { return []byte("not json"), nil })
	if _, ok := ItemsOK("get", "pods"); ok {
		t.Error("kItemsOK on unparseable body: want answered=false")
	}
}
