package harborauth

// probes_test.go — the two "is it here" probes, exercised through the exec seam.
//
// They are three lines each and were previously covered only by being REPLACED in
// their callers' tests, which means the argv they build — the part that decides
// whether the answer is right — was never read by anything. That matters more than
// usual here: RobotExternalSecretExists exists because NamespaceExists stopped
// being a valid deployment probe, so a typo in its resource kind would silently
// restore the bug it was added to fix.

import (
	"errors"
	"strings"
	"testing"
)

func withExec(t *testing.T, out []byte, err error) *[]string {
	t.Helper()
	orig := execOutput
	t.Cleanup(func() { execOutput = orig })
	var got []string
	execOutput = func(name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return out, err
	}
	return &got
}

func TestRobotExternalSecretExistsAsksForAnExternalSecret(t *testing.T) {
	argv := withExec(t, []byte("externalsecret.external-secrets.io/harbor-docker-config\n"), nil)
	present, err := RobotExternalSecretExists("llz-cert-automation", "harbor-docker-config")
	if err != nil || !present {
		t.Fatalf("present=%v err=%v, want true/nil", present, err)
	}
	joined := strings.Join(*argv, " ")
	// The RESOURCE KIND is the whole point: probing the namespace (or the Secret)
	// is what made this gate wrong on managed.
	if !strings.Contains(joined, "get externalsecret harbor-docker-config") ||
		!strings.Contains(joined, "-n llz-cert-automation") {
		t.Errorf("argv = %q, want an externalsecret lookup in the component namespace", joined)
	}
	if !strings.Contains(joined, "--ignore-not-found") {
		t.Errorf("argv = %q — without --ignore-not-found an absent object is an ERROR, and this probe "+
			"must be able to answer 'no' without failing", joined)
	}
}

func TestRobotExternalSecretExistsReportsAbsence(t *testing.T) {
	withExec(t, []byte("  \n"), nil)
	present, err := RobotExternalSecretExists("ns", "name")
	if err != nil || present {
		t.Errorf("empty output means absent; got present=%v err=%v", present, err)
	}
}

// A read ERROR is not an answer. Reporting "absent" on a broken kubectl would
// silently skip the lane on every cluster whose API was briefly unreachable.
func TestRobotExternalSecretExistsPropagatesErrors(t *testing.T) {
	withExec(t, nil, errors.New("connection refused"))
	if _, err := RobotExternalSecretExists("ns", "name"); err == nil {
		t.Error("a failed probe must return an error, not a confident 'not deployed'")
	}
}

func TestNamespaceExistsAsksForANamespace(t *testing.T) {
	argv := withExec(t, []byte("namespace/llz-cert-automation\n"), nil)
	present, err := NamespaceExists("llz-cert-automation")
	if err != nil || !present {
		t.Fatalf("present=%v err=%v, want true/nil", present, err)
	}
	if joined := strings.Join(*argv, " "); !strings.Contains(joined, "get namespace llz-cert-automation") {
		t.Errorf("argv = %q, want a namespace lookup", joined)
	}
}
