package main

import (
	"testing"
)

// clearOpenbaoEnv blanks every OPENBAO_* var openbaoClientForward reads so a test
// starts from a known-empty environment (t.Setenv restores them on cleanup).
func clearOpenbaoEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OPENBAO_ADDR_ACTIVE", "OPENBAO_ADDR_STANDBY",
		"OPENBAO_TOKEN_ACTIVE", "OPENBAO_TOKEN_STANDBY", "OPENBAO_TOKEN",
		"OPENBAO_ROOT_TOKEN", "OPENBAO_NAMESPACE",
	} {
		t.Setenv(k, "")
	}
}

// seamForward stubs portForwardOpenbaoFn and records whether it was invoked.
func seamForward(t *testing.T, addr string, err error) *bool {
	t.Helper()
	called := false
	orig := portForwardOpenbaoFn
	portForwardOpenbaoFn = func() (string, func(), error) {
		called = true
		return addr, func() {}, err
	}
	t.Cleanup(func() { portForwardOpenbaoFn = orig })
	return &called
}

func TestOpenbaoClientForward_ExplicitAddrWins(t *testing.T) {
	clearOpenbaoEnv(t)
	t.Setenv("OPENBAO_ADDR_ACTIVE", "https://bao.example:8200")
	t.Setenv("OPENBAO_TOKEN", "s.token")
	called := seamForward(t, "https://127.0.0.1:1", nil)

	c, cleanup, err := openbaoClientForward(roleActive)
	if err != nil || c == nil {
		t.Fatalf("openbaoClientForward = (%v, %v), want a client", c, err)
	}
	cleanup()
	if *called {
		t.Error("port-forward was opened despite OPENBAO_ADDR_ACTIVE being set")
	}
}

func TestOpenbaoClientForward_StandaloneAutoForwards(t *testing.T) {
	clearOpenbaoEnv(t)
	t.Setenv("OPENBAO_TOKEN", "s.token") // addr unset, no standby → standalone
	called := seamForward(t, "https://127.0.0.1:34567", nil)

	c, cleanup, err := openbaoClientForward(roleActive)
	if err != nil || c == nil {
		t.Fatalf("openbaoClientForward = (%v, %v), want a client", c, err)
	}
	defer cleanup()
	if !*called {
		t.Error("standalone with no addr should have opened a port-forward")
	}
}

func TestOpenbaoClientForward_RootTokenAccepted(t *testing.T) {
	clearOpenbaoEnv(t)
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root") // the regen-root operator flow
	called := seamForward(t, "https://127.0.0.1:34567", nil)

	if _, cleanup, err := openbaoClientForward(roleActive); err != nil {
		t.Fatalf("openbaoClientForward with OPENBAO_ROOT_TOKEN only = %v, want ok", err)
	} else {
		defer cleanup()
	}
	if !*called {
		t.Error("OPENBAO_ROOT_TOKEN should satisfy the token requirement for auto-forward")
	}
}

func TestOpenbaoClientForward_NoTokenErrors(t *testing.T) {
	clearOpenbaoEnv(t) // addr unset AND no token
	called := seamForward(t, "https://127.0.0.1:1", nil)

	if _, _, err := openbaoClientForward(roleActive); err == nil {
		t.Error("openbaoClientForward with no addr and no token = nil, want error")
	}
	if *called {
		t.Error("port-forward should not open when no token is available")
	}
}

func TestOpenbaoClientForward_HAActiveDoesNotForward(t *testing.T) {
	clearOpenbaoEnv(t)
	// A standby is configured (HA pair) but the active addr is unset. The operator
	// addresses HA explicitly, so this must NOT auto-forward — it keeps the
	// "OPENBAO_ADDR_ACTIVE is not set" error.
	t.Setenv("OPENBAO_ADDR_STANDBY", "https://bao-standby.example:8200")
	t.Setenv("OPENBAO_TOKEN", "s.token")
	called := seamForward(t, "https://127.0.0.1:1", nil)

	if _, _, err := openbaoClientForward(roleActive); err == nil {
		t.Error("HA active with unset addr = nil, want the not-set error")
	}
	if *called {
		t.Error("HA deployment should not auto port-forward the active")
	}
}

func TestOpenbaoClientForward_StandbyNeverForwards(t *testing.T) {
	clearOpenbaoEnv(t)
	t.Setenv("OPENBAO_TOKEN", "s.token")
	called := seamForward(t, "https://127.0.0.1:1", nil)

	if _, _, err := openbaoClientForward(roleStandby); err == nil {
		t.Error("standby with unset addr = nil, want the not-set error")
	}
	if *called {
		t.Error("standby role should never auto port-forward")
	}
}
