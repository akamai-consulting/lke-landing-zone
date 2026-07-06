package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaoConfigureStepsShape(t *testing.T) {
	steps := baoConfigureSteps("acme/platform")
	if len(steps) != 18 {
		t.Fatalf("got %d steps, want 18 (14 base + 4 GitHub-OIDC: jwt enable, jwt config, 2 roles)", len(steps))
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
	if n := len(baoConfigureSteps("")); n != 14 {
		t.Errorf("no-repo configure should omit JWT steps: got %d, want 14", n)
	}
	// SECURITY: every jwt role must pin to the instance repo + owner audience.
	// Two roles expected: platform-ci (read) and secret-propagator (write). The
	// role body is JSON over stdin (`write <path> -`) so bound_claims is a typed
	// map, not a key=value string the API rejects — assert against the stdin JSON.
	jwtRolePolicy := map[string]string{}
	for _, s := range steps {
		if len(s.args) >= 3 && s.args[0] == "write" && strings.HasPrefix(s.args[1], "auth/jwt/role/") {
			if s.args[len(s.args)-1] != "-" || s.stdin == "" {
				t.Errorf("jwt role %s must write its JSON body over stdin (got args %v, stdin %q)", s.args[1], s.args, s.stdin)
				continue
			}
			var body struct {
				BoundAudiences []string          `json:"bound_audiences"`
				BoundClaims    map[string]string `json:"bound_claims"`
				TokenPolicies  []string          `json:"token_policies"`
			}
			if err := json.Unmarshal([]byte(s.stdin), &body); err != nil {
				t.Errorf("jwt role %s stdin is not valid JSON: %v", s.args[1], err)
				continue
			}
			if body.BoundClaims["repository"] != "acme/platform" {
				t.Errorf("jwt role %s must bound_claims the instance repo; got %v", s.args[1], body.BoundClaims)
			}
			if len(body.BoundAudiences) != 1 || body.BoundAudiences[0] != "https://github.com/acme" {
				t.Errorf("jwt role %s must bound_audiences the owner; got %v", s.args[1], body.BoundAudiences)
			}
			role := strings.TrimPrefix(s.args[1], "auth/jwt/role/")
			if len(body.TokenPolicies) == 1 {
				jwtRolePolicy[role] = body.TokenPolicies[0]
			}
		}
	}
	if jwtRolePolicy["platform-ci"] != "platform-ci" || jwtRolePolicy["secret-propagator"] != "secret-propagator" {
		t.Errorf("jwt roles = %v, want platform-ci->platform-ci and secret-propagator->secret-propagator", jwtRolePolicy)
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
	if strings.Join(policies, ",") != "platform-ci,secret-propagator,eso-pusher,linode-rotator,harbor-provisioner,reconciler-read" {
		t.Errorf("policies = %v", policies)
	}
}

// The in-cluster reconciler's Kubernetes-auth role must bind every policy the
// CronJobs it replaces used: reconciler-read (gauge metadata read), linode-rotator
// (object-storage-key read_write, --reconcile-linode-creds), and harbor-provisioner
// (secret/harbor/{robot,pull-robot} read_write, --reconcile-harbor). It must NOT
// bind the harbor admin path (that stays ESO-only via harbor-admin-push).
func TestReconcilerRoleBindsDrivingPolicies(t *testing.T) {
	var found bool
	for _, s := range baoConfigureSteps("acme/platform") {
		if len(s.args) >= 2 && s.args[0] == "write" && s.args[1] == "auth/kubernetes/role/reconciler" {
			found = true
			joined := strings.Join(s.args, " ")
			if !strings.Contains(joined, "policies=reconciler-read,linode-rotator,harbor-provisioner") {
				t.Errorf("reconciler role must bind reconciler-read + linode-rotator + harbor-provisioner; got %v", s.args)
			}
		}
	}
	if !found {
		t.Fatal("no auth/kubernetes/role/reconciler step found")
	}
}

// TestBaoConfigureJWTBoundClaimsIsMap is an explicit regression guard for the
// 2026-06-25 kube-native failure where the JWT role write emitted bound_claims
// as an empty STRING (key=value CLI args) and OpenBao's auth/jwt rejected it:
//
//	Code: 400 … error converting input for field "bound_claims":
//	'' expected type 'map[string]interface {}', got unconvertible type 'string'
//
// The fix writes the role body as JSON over stdin so bound_claims is a typed
// object. Assert that against the RAW JSON (not a typed struct that would only
// fail incidentally) so a regression to key=value args trips a clear message.
func TestBaoConfigureJWTBoundClaimsIsMap(t *testing.T) {
	roles := 0
	for _, s := range baoConfigureSteps("acme/platform") {
		if !(len(s.args) >= 2 && s.args[0] == "write" && strings.HasPrefix(s.args[1], "auth/jwt/role/")) {
			continue
		}
		roles++
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s.stdin), &raw); err != nil {
			t.Fatalf("%s: stdin is not a JSON object: %v", s.args[1], err)
		}
		bc, ok := raw["bound_claims"]
		if !ok {
			t.Errorf("%s: bound_claims missing (role would bind to ANY repo)", s.args[1])
			continue
		}
		var asMap map[string]any
		if err := json.Unmarshal(bc, &asMap); err != nil {
			t.Errorf("%s: bound_claims must be a JSON object/map, got %s — the 2026-06-25 '\\'\\' expected map, got string' regression", s.args[1], bc)
		} else if asMap["repository"] != "acme/platform" {
			t.Errorf("%s: bound_claims.repository = %v, want acme/platform", s.args[1], asMap["repository"])
		}
	}
	if roles != 2 {
		t.Fatalf("expected 2 jwt roles (platform-ci, secret-propagator), saw %d", roles)
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
	if !strings.Contains(policySecretPropagator, `path "secret/data/linode/api-token"`) {
		t.Error("secret-propagator policy missing the linode api-token path")
	}
	// eso-pusher must grant create/update (push) on exactly the in-cluster-sourced
	// paths (grafana admin, otel bearer, harbor admin) and nothing else; a wider
	// grant would over-privilege the ESO SA.
	for _, p := range []string{
		`path "secret/data/grafana/admin"`,
		`path "secret/data/otel/ingress"`,
		`path "secret/data/harbor/admin"`,
	} {
		if !strings.Contains(policyESOPusher, p) {
			t.Errorf("eso-pusher policy missing %s", p)
		}
	}
	// The metadata paths must grant create/update, not just read: ESO stamps the
	// managed-by custom_metadata on first push (PUT secret/metadata/<path>), so a
	// read-only metadata grant 403s the first PushSecret and wedges convergence.
	for _, p := range []string{
		`path "secret/metadata/grafana/admin" { capabilities = ["create", "update", "read"] }`,
		`path "secret/metadata/otel/ingress"  { capabilities = ["create", "update", "read"] }`,
		`path "secret/metadata/harbor/admin"  { capabilities = ["create", "update", "read"] }`,
	} {
		if !strings.Contains(policyESOPusher, p) {
			t.Errorf("eso-pusher policy must grant create/update on metadata path: %s", p)
		}
	}
	for _, forbidden := range []string{"linode/api-token", "harbor/registry-s3", "loki/object-store", `"*"`} {
		if strings.Contains(policyESOPusher, forbidden) {
			t.Errorf("eso-pusher policy is over-scoped: contains %q", forbidden)
		}
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
	// lookup + 18 steps (14 base + 4 GitHub-OIDC) + audit list.
	if len(calls) != 20 {
		t.Fatalf("got %d bao calls, want 20: %v", len(calls), calls)
	}
	if calls[0] != "token lookup -format=json" || calls[19] != "audit list" {
		t.Errorf("unexpected first/last calls: %q / %q", calls[0], calls[19])
	}
	// The jwt role must actually be written during the run (body is JSON over
	// stdin; repo/audience binding is asserted in TestBaoConfigureStepsShape).
	var sawJWT bool
	for _, c := range calls {
		if strings.Contains(c, "write auth/jwt/role/platform-ci -") {
			sawJWT = true
		}
	}
	if !sawJWT {
		t.Errorf("expected a jwt role write; calls=%v", calls)
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
		if strings.HasPrefix(c, "write auth/kubernetes/role") {
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
