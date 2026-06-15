package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaoConfigureStepsShape(t *testing.T) {
	steps := baoConfigureSteps()
	if len(steps) != 12 {
		t.Fatalf("got %d steps, want 12 (3 enables, k8s config, 3 policies, 4 role writes, 1 k8s role)", len(steps))
	}
	// The three enables are the only non-fatal steps (the bash `|| true`).
	for i, s := range steps {
		wantFatal := i >= 3
		if s.fatal != wantFatal {
			t.Errorf("step %d (%s): fatal=%v, want %v", i, s.desc, s.fatal, wantFatal)
		}
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
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("GITHUB_ENV", envFile)
	var calls []string
	withBaoExec(t, configureStub(t, &calls, nil))

	if err := runCIBaoConfigure(globalOpts{}, "primary"); err != nil {
		t.Fatal(err)
	}
	// lookup + 12 steps + audit list.
	if len(calls) != 14 {
		t.Fatalf("got %d bao calls, want 14: %v", len(calls), calls)
	}
	if calls[0] != "token lookup -format=json" || calls[13] != "audit list" {
		t.Errorf("unexpected first/last calls: %q / %q", calls[0], calls[13])
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
