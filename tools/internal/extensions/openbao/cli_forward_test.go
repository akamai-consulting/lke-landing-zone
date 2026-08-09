package openbao

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/envtopology"
)

// clearOpenbaoEnv blanks every OPENBAO_* var ClientForward reads so a test
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

// seamForward stubs PortForwardFn and records whether it was invoked.
func seamForward(t *testing.T, addr string, err error) *bool {
	t.Helper()
	called := false
	orig := PortForwardFn
	PortForwardFn = func() (string, func(), error) {
		called = true
		return addr, func() {}, err
	}
	t.Cleanup(func() { PortForwardFn = orig })
	return &called
}

func TestOpenbaoClientForward_ExplicitAddrWins(t *testing.T) {
	clearOpenbaoEnv(t)
	t.Setenv("OPENBAO_ADDR_ACTIVE", "https://bao.example:8200")
	t.Setenv("OPENBAO_TOKEN", "s.token")
	called := seamForward(t, "https://127.0.0.1:1", nil)

	c, cleanup, err := ClientForward(envtopology.RoleActive)
	if err != nil || c == nil {
		t.Fatalf("ClientForward = (%v, %v), want a client", c, err)
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

	c, cleanup, err := ClientForward(envtopology.RoleActive)
	if err != nil || c == nil {
		t.Fatalf("ClientForward = (%v, %v), want a client", c, err)
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

	var cleanup func()
	stderr := captureStderr(t, func() {
		var err error
		var c *Client
		if c, cleanup, err = ClientForward(envtopology.RoleActive); err != nil {
			t.Fatalf("ClientForward with OPENBAO_ROOT_TOKEN only = %v, want ok", err)
		}
		_ = c
	})
	if cleanup != nil {
		defer cleanup()
	}
	if !*called {
		t.Error("OPENBAO_ROOT_TOKEN should satisfy the token requirement for auto-forward")
	}
	// Root works, but the operator must be nudged toward `llz openbao login`.
	if !strings.Contains(stderr, "ROOT token") || !strings.Contains(stderr, "llz openbao login") {
		t.Errorf("falling back to OPENBAO_ROOT_TOKEN should warn + steer to login; stderr = %q", stderr)
	}
}

func TestOpenbaoClientForward_TeamTokenNoWarn(t *testing.T) {
	clearOpenbaoEnv(t)
	t.Setenv("OPENBAO_TOKEN", "s.team") // a team-scoped token from `llz openbao login`
	seamForward(t, "https://127.0.0.1:34567", nil)

	var cleanup func()
	stderr := captureStderr(t, func() {
		_, cleanup, _ = ClientForward(envtopology.RoleActive)
	})
	if cleanup != nil {
		defer cleanup()
	}
	if strings.Contains(stderr, "ROOT token") {
		t.Errorf("a team-scoped OPENBAO_TOKEN must not trigger the root-token warning; stderr = %q", stderr)
	}
}

func TestOpenbaoClientForward_AllowRootSilencesWarn(t *testing.T) {
	clearOpenbaoEnv(t)
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	t.Setenv("OPENBAO_ALLOW_ROOT", "1") // escape hatch for root-only automation
	seamForward(t, "https://127.0.0.1:34567", nil)

	var cleanup func()
	stderr := captureStderr(t, func() {
		_, cleanup, _ = ClientForward(envtopology.RoleActive)
	})
	if cleanup != nil {
		defer cleanup()
	}
	if strings.Contains(stderr, "ROOT token") {
		t.Errorf("OPENBAO_ALLOW_ROOT should silence the root-token warning; stderr = %q", stderr)
	}
}

func TestOpenbaoClientForward_NoTokenErrors(t *testing.T) {
	clearOpenbaoEnv(t) // addr unset AND no token
	called := seamForward(t, "https://127.0.0.1:1", nil)

	if _, _, err := ClientForward(envtopology.RoleActive); err == nil {
		t.Error("ClientForward with no addr and no token = nil, want error")
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

	if _, _, err := ClientForward(envtopology.RoleActive); err == nil {
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

	if _, _, err := ClientForward(envtopology.RoleStandby); err == nil {
		t.Error("standby with unset addr = nil, want the not-set error")
	}
	if *called {
		t.Error("standby role should never auto port-forward")
	}
}
