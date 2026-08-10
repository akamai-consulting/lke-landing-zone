package assertidentity

// deps_fill_test.go — an Install that omits a field must not leave it nil.
//
// THE SECOND INSTANCE OF THIS DEFECT, three assert-suite rounds after the first
// (assertreconciler.WithPrometheus). internal/cli's literal omitted
// PortForwardOpenbao, Install was a wholesale `caps = d`, and the team-write lane
// SIGSEGV'd at loginsmoke.go:194 — after completing the ENTIRE Keycloak half:
// admin creds read, realm role granted, id_token groups verified, test user and
// client torn down. Everything the lane exists to prove had passed; it crashed
// handing the result to OpenBao.
//
// internal/cli/deps_literals_test.go catches the omission at PR time. This is the
// belt: even an omission that slips through must degrade to a NAMED ERROR.

import (
	"strings"
	"testing"
)

func TestInstallFillsOmittedSeams(t *testing.T) {
	orig := caps
	t.Cleanup(func() { caps = orig })

	// The exact shape internal/cli shipped: PortForwardOpenbao omitted.
	Install(Deps{
		Exec:        func(string, ...string) ([]byte, error) { return nil, nil },
		SecretField: func(string, string, string) string { return "" },
	})

	if caps.PortForwardOpenbao == nil {
		t.Fatal("Install left PortForwardOpenbao nil — calling it is the SIGSEGV that killed team-write mid-e2e")
	}
	_, _, err := caps.PortForwardOpenbao()
	if err == nil {
		t.Fatal("the filled default must FAIL CLOSED — returning an empty address with a nil error " +
			"hands the lane a blank host to dial, which fails later and less legibly")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("the error should name the un-installed seam, got %v", err)
	}
	// The fields the caller DID set must survive the fill.
	if caps.Exec == nil || caps.SecretField == nil {
		t.Error("filling must not clobber what the caller provided")
	}
}
