package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaoConfigureStepsShape(t *testing.T) {
	steps := baoConfigureSteps("acme/platform")
	if len(steps) != 15 {
		t.Fatalf("got %d steps, want 15 (12 base + 3 GitHub-OIDC: jwt enable, jwt config, jwt role)", len(steps))
	}
	// `enable` steps are the only non-fatal ones (the bash `|| true`) — check by
	// shape, not index, so adding a new enable (jwt) can't silently violate it.
	for _, s := range steps {
		isEnable := len(s.args) >= 2 && s.args[1] == "enable"
		if s.fatal == isEnable {
			t.Errorf("step %q: fatal=%v but isEnable=%v (enables are the only non-fatal steps)", s.desc, s.fatal, isEnable)
		}
	}
	// A repo-less configure omits the GitHub-OIDC steps entirely.
	if n := len(baoConfigureSteps("")); n != 12 {
		t.Errorf("no-repo configure should omit JWT steps: got %d, want 12", n)
	}
	// SECURITY: the jwt role must pin to the instance repo and map to platform-ci.
	var sawJWTRole bool
	for _, s := range steps {
		if len(s.args) > 1 && s.args[0] == "write" && strings.HasPrefix(s.args[1], "auth/jwt/role/") {
			sawJWTRole = true
			joined := strings.Join(s.args, " ")
			if !strings.Contains(joined, `bound_claims={"repository":"acme/platform"}`) {
				t.Errorf("jwt role must bound_claims the instance repo; got %v", s.args)
			}
			if !strings.Contains(joined, "bound_audiences=https://github.com/acme") {
				t.Errorf("jwt role must bound_audiences the owner; got %v", s.args)
			}
			if !strings.Contains(joined, "token_policies=platform-ci") {
				t.Errorf("jwt role must map to platform-ci; got %v", s.args)
			}
		}
	}
	if !sawJWTRole {
		t.Error("expected a jwt role step when ghRepo is set")
	}
	// Policy writes deliver the document over stdin to `policy write <name> -`.
	var policies []string
	for _, s := range steps {
		if len(s.args) > 1 && s.args[0] == "policy" {
			if s.args[len(s.args)-1] != "-" || s.stdin == "" {
				t.Errorf("policy step %q must read the document from stdin", s.desc)
			}
			policies = append(policies, s.args[2])
		}
	}
	if strings.Join(policies, ",") != "platform-ci,approle-rotator,secret-propagator" {
		t.Errorf("policies = %v", policies)
	}
}

func TestPolicyDocuments(t *testing.T) {
	// Spot-check load-bearing paths so an accidental edit trips a test.
	for _, p := range []string{
		`path "secret/data/loki/object-store"`,
		`path "secret/metadata/loki/object-store"`,
		`path "secret/data/harbor/registry-s3"`,
	} {
		if !strings.Contains(policyPlatformCI, p) {
			t.Errorf("platform-ci policy missing %s", p)
		}
	}
	if !strings.Contains(policyAppRoleRotator, "secret-propagator/secret-id-accessor/destroy") {
		t.Error("approle-rotator policy missing the secret-propagator destroy path")
	}
	if !strings.Contains(policySecretPropagator, `path "secret/data/linode/api-token"`) {
		t.Error("secret-propagator policy missing the linode api-token path")
	}
}

func TestAuditFileDeviceActive(t *testing.T) {
	active := "Path     Type    Description\n----     ----    -----------\nfile/    file    n/a\n"
	if !auditFileDeviceActive(active) {
		t.Error("file/ row not recognized")
	}
	for _, out := range []string{"", "No audit devices are enabled.\n", "syslog/  syslog  n/a\n"} {
		if auditFileDeviceActive(out) {
			t.Errorf("auditFileDeviceActive(%q) = true, want false", out)
		}
	}
}

// configureStub returns a baoExecFn stub with per-command behavior overrides.
func configureStub(t *testing.T, calls *[]string, override func(cmd string) (string, string, error, bool)) func(pod, token, stdin string, args ...string) (string, string, error) {
	t.Helper()
	return func(pod, token, stdin string, args ...string) (string, string, error) {
		cmd := strings.Join(args, " ")
		*calls = append(*calls, cmd)
		if token != "s.root" {
			t.Errorf("%q ran with token %q, want the root token", cmd, token)
		}
		if override != nil {
			if out, errOut, err, hit := override(cmd); hit {
				return out, errOut, err
			}
		}
		switch {
		case strings.HasPrefix(cmd, "token lookup"):
			return `{"data":{"policies":["root"]}}`, "", nil
		case strings.HasPrefix(cmd, "audit list"):
			return "file/    file    n/a\n", "", nil
		}
		return "", "", nil
	}
}

func TestRunCIBaoConfigureHappyPath(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	// Pin GITHUB_REPOSITORY so the run is deterministic regardless of whether the
	// environment provides one (GitHub Actions auto-sets it) — with it set, the
	// GitHub-OIDC (jwt) steps are appended, exercising that execution path.
	t.Setenv("GITHUB_REPOSITORY", "acme/platform")
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("GITHUB_ENV", envFile)
	var calls []string
	withBaoExec(t, configureStub(t, &calls, nil))

	if err := runCIBaoConfigure(globalOpts{}, "primary"); err != nil {
		t.Fatal(err)
	}
	// lookup + 15 steps (12 base + 3 GitHub-OIDC) + audit list.
	if len(calls) != 17 {
		t.Fatalf("got %d bao calls, want 17: %v", len(calls), calls)
	}
	if calls[0] != "token lookup -format=json" || calls[16] != "audit list" {
		t.Errorf("unexpected first/last calls: %q / %q", calls[0], calls[16])
	}
	// The repo-bound jwt role must actually be written during the run.
	var sawJWT bool
	for _, c := range calls {
		if strings.Contains(c, "write auth/jwt/role/platform-ci") && strings.Contains(c, `bound_claims={"repository":"acme/platform"}`) {
			sawJWT = true
		}
	}
	if !sawJWT {
		t.Errorf("expected a repo-bound jwt role write; calls=%v", calls)
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		b, _ := os.ReadFile(envFile)
		t.Errorf("healthy run wrote GITHUB_ENV %q, want nothing", b)
	}
}

func TestRunCIBaoConfigureEnablesTolerateExisting(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	var calls []string
	withBaoExec(t, configureStub(t, &calls, func(cmd string) (string, string, error, bool) {
		if strings.HasPrefix(cmd, "secrets enable") || strings.HasPrefix(cmd, "auth enable") {
			return "", "Error enabling: path is already in use at secret/", errors.New("exit status 2"), true
		}
		return "", "", nil, false
	}))
	if err := runCIBaoConfigure(globalOpts{}, "primary"); err != nil {
		t.Fatalf("re-run with existing mounts must succeed, got %v", err)
	}
}

func TestRunCIBaoConfigureFatalStepAborts(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	var calls []string
	withBaoExec(t, configureStub(t, &calls, func(cmd string) (string, string, error, bool) {
		if strings.HasPrefix(cmd, "policy write platform-ci") {
			return "", "Code: 503. * Vault is sealed", errors.New("exit status 2"), true
		}
		return "", "", nil, false
	}))
	err := runCIBaoConfigure(globalOpts{}, "primary")
	if err == nil || !strings.Contains(err.Error(), "platform-ci") {
		t.Errorf("err = %v, want fatal policy-write failure", err)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "write auth/approle") {
			t.Errorf("steps after the fatal failure still ran: %q", c)
		}
	}
}

func TestRunCIBaoConfigureInvalidToken(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.stale")
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		if args[0] != "token" {
			t.Errorf("preflight failure must stop everything, ran %v", args)
		}
		return "", "Code: 403. * permission denied", errors.New("exit status 2")
	})
	if err := runCIBaoConfigure(globalOpts{}, "primary"); err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Errorf("err = %v, want preflight failure", err)
	}
}

func TestRunCIBaoConfigureNonRootToken(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.limited")
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		if args[0] != "token" {
			t.Errorf("non-root token must stop everything, ran %v", args)
		}
		return `{"data":{"policies":["platform-ci","default"]}}`, "", nil
	})
	if err := runCIBaoConfigure(globalOpts{}, "primary"); err == nil || !strings.Contains(err.Error(), "not root") {
		t.Errorf("err = %v, want not-root refusal", err)
	}
}

func TestRunCIBaoConfigureMissingAuditDeviceWarnsNotFails(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.root")
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("GITHUB_ENV", envFile)
	var calls []string
	withBaoExec(t, configureStub(t, &calls, func(cmd string) (string, string, error, bool) {
		if strings.HasPrefix(cmd, "audit list") {
			return "No audit devices are enabled.\n", "", nil, true
		}
		return "", "", nil, false
	}))
	if err := runCIBaoConfigure(globalOpts{}, "primary"); err != nil {
		t.Fatalf("missing audit device must warn, not fail: %v", err)
	}
	b, _ := os.ReadFile(envFile)
	if string(b) != "BOOTSTRAP_ERRORS=true\n" {
		t.Errorf("GITHUB_ENV = %q, want BOOTSTRAP_ERRORS=true", b)
	}
}
