package identityconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

func TestBaoConfigureStepsShape(t *testing.T) {
	steps := baoConfigureSteps("acme/platform", "", nil)
	if len(steps) != 19 {
		t.Fatalf("got %d steps, want 19 (15 base + 4 GitHub-OIDC: jwt enable, jwt config, 2 roles)", len(steps))
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
	if n := len(baoConfigureSteps("", "", nil)); n != 15 {
		t.Errorf("no-repo configure should omit JWT steps: got %d, want 15", n)
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
	if strings.Join(policies, ",") != "platform-ci,secret-propagator,eso-pusher,linode-rotator,harbor-provisioner,reconciler-read,broad-pat-rotator" {
		t.Errorf("policies = %v", policies)
	}
}

// The in-cluster reconciler's Kubernetes-auth role binds reconciler-read (gauge
// metadata read, --reconcile-openbao-gauges) + linode-rotator (object-storage-key
// read_write, --reconcile-linode-creds; it took over the linodeCredRotator CronJob).
// It must NOT bind harbor-provisioner: harbor provisioning stays on the in-namespace
// harbor-robot-provisioner CronJob (its own SA-bound harbor-provisioner role), because
// the reconciler can't reach mesh-protected harbor-core from the llz-reconciler namespace.
func TestReconcilerRoleBindsDrivingPolicies(t *testing.T) {
	var reconcilerFound, harborRoleFound bool
	for _, s := range baoConfigureSteps("acme/platform", "", nil) {
		if len(s.args) < 2 || s.args[0] != "write" {
			continue
		}
		joined := strings.Join(s.args, " ")
		switch s.args[1] {
		case "auth/kubernetes/role/reconciler":
			reconcilerFound = true
			if !strings.Contains(joined, "policies=reconciler-read,linode-rotator ") {
				t.Errorf("reconciler role must bind exactly reconciler-read + linode-rotator; got %v", s.args)
			}
			if strings.Contains(joined, "harbor-provisioner") {
				t.Error("reconciler role must NOT bind harbor-provisioner — harbor stays a CronJob")
			}
		case "auth/kubernetes/role/harbor-provisioner":
			harborRoleFound = true
			if !strings.Contains(joined, "bound_service_account_names=harbor-robot-provisioner") {
				t.Errorf("harbor-provisioner role must bind the harbor-robot-provisioner SA; got %v", s.args)
			}
		}
	}
	if !reconcilerFound {
		t.Fatal("no auth/kubernetes/role/reconciler step found")
	}
	if !harborRoleFound {
		t.Fatal("no auth/kubernetes/role/harbor-provisioner step found (harbor CronJob needs it)")
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
	for _, s := range baoConfigureSteps("acme/platform", "", nil) {
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

// TestKeycloakTeamSteps covers the Phase-1 human-operator write path: a second
// jwt auth mount at keycloak/ + a per-team `<name>-writer` policy and a role
// bound to the team's Keycloak group. See docs/designs/team-scoped-credentials.md.
func TestKeycloakTeamSteps(t *testing.T) {
	issuer := "https://keycloak.example/realms/otomi"
	// No issuer or no teams → no steps, so a domain-less or team-less instance is
	// byte-identical to before.
	if s := keycloakTeamSteps("", []clusterspec.Team{{Name: "gsap", OpenbaoSubtree: "secret/gsap"}}); s != nil {
		t.Errorf("no issuer must yield no steps, got %d", len(s))
	}
	if s := keycloakTeamSteps(issuer, nil); s != nil {
		t.Errorf("no teams must yield no steps, got %d", len(s))
	}

	teams := []clusterspec.Team{
		{Name: "gsap", OpenbaoSubtree: "secret/gsap"},
		{Name: "web", OpenbaoSubtree: "secret/web"},
	}
	steps := keycloakTeamSteps(issuer, teams)
	if len(steps) != 6 { // enable + config + (policy+role)*2
		t.Fatalf("got %d steps, want 6", len(steps))
	}
	// The mount `enable` is the only non-fatal step (matches the bash `|| true`).
	if steps[0].fatal || len(steps[0].args) < 2 || steps[0].args[1] != "enable" ||
		!strings.Contains(strings.Join(steps[0].args, " "), "-path=keycloak") {
		t.Errorf("step0 must be the non-fatal `auth enable -path=keycloak`, got %+v", steps[0])
	}
	// The mount validates via a jwks_url on the SAME HOST as the public issuer —
	// that host is the only name apl-core has a certificate for (the gateway's
	// `otomi-wildcard`), and the in-cluster route to it is the hostAliases pin
	// from `llz ci pin-keycloak-gateway-alias`. Still NOT oidc_discovery_url.
	step1 := strings.Join(steps[1].args, " ")
	if steps[1].args[1] != "auth/keycloak/config" ||
		!strings.Contains(step1, "jwks_url="+keycloakJWKSURL(issuer)) ||
		!strings.Contains(step1, "bound_issuer="+issuer) {
		t.Errorf("step1 must configure jwks_url (issuer-derived) + bound_issuer (public), got %v", steps[1].args)
	}
	// A Service DNS name here is the bug this replaced: nothing in the cluster
	// holds a certificate for one, so the fetch either runs in plaintext or hits
	// a port with no TLS listener at all.
	if strings.Contains(step1, ".svc.cluster.local") {
		t.Errorf("step1 must not point jwks_url at a Service DNS name — no cert covers it: %v", steps[1].args)
	}
	if strings.Contains(step1, "oidc_discovery_url") {
		t.Errorf("step1 must NOT use oidc_discovery_url (the public URL hairpins in-cluster): %v", steps[1].args)
	}

	rolePolicy := map[string]string{}
	roleGroup := map[string][]string{}
	policyDoc := map[string]string{}
	for _, s := range steps[2:] {
		if !s.fatal {
			t.Errorf("team step %q must be fatal", s.desc)
		}
		switch {
		case s.args[0] == "policy" && s.args[1] == "write":
			if s.args[len(s.args)-1] != "-" || s.stdin == "" {
				t.Errorf("policy %s must be written over stdin", s.args[2])
			}
			policyDoc[s.args[2]] = s.stdin
		case s.args[0] == "write" && strings.HasPrefix(s.args[1], "auth/keycloak/role/"):
			var body keycloakRoleBody
			if err := json.Unmarshal([]byte(s.stdin), &body); err != nil {
				t.Fatalf("role %s body not JSON: %v", s.args[1], err)
			}
			name := strings.TrimPrefix(s.args[1], "auth/keycloak/role/")
			if len(body.TokenPolicies) == 1 {
				rolePolicy[name] = body.TokenPolicies[0]
			}
			roleGroup[name] = body.BoundClaims["groups"]
			if body.UserClaim != "sub" {
				t.Errorf("role %s user_claim = %q, want sub (attribution)", name, body.UserClaim)
			}
			// The role MUST pin the audience to the llz client, or any realm token
			// carrying the groups claim (Grafana/Harbor/console) would be accepted.
			if len(body.BoundAudiences) != 1 || body.BoundAudiences[0] != DeviceClientID {
				t.Errorf("role %s bound_audiences = %v, want [%s] (audience-confusion guard)", name, body.BoundAudiences, DeviceClientID)
			}
		}
	}
	if rolePolicy["gsap"] != "gsap-writer" || rolePolicy["web"] != "web-writer" {
		t.Errorf("role→policy = %v, want gsap->gsap-writer, web->web-writer", rolePolicy)
	}
	// The role binds on the apl-core realm role team-<name> (the value apl-core's
	// default groups claim carries), NOT the bare team name — AND on platform-admin,
	// apl-core's built-in all-teams admin role, so a platform admin can write any
	// team's subtree. (platform-admin, NOT team-admin: the default admin carries
	// platform-admin; team-admin is the apl admin *team* role — see the bind comment.)
	wantGroups := map[string][]string{
		"gsap": {"team-gsap", "platform-admin"},
		"web":  {"team-web", "platform-admin"},
	}
	for name, want := range wantGroups {
		got := roleGroup[name]
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("role %s → groups = %v, want %v (team role + platform-admin)", name, got, want)
		}
	}
	// The writer policy grants create/update on data/* and read+list on metadata/*.
	if !strings.Contains(policyDoc["gsap-writer"], `path "secret/data/gsap/*" { capabilities = ["create", "update", "read"] }`) {
		t.Errorf("gsap-writer policy missing data write path:\n%s", policyDoc["gsap-writer"])
	}
	if !strings.Contains(policyDoc["gsap-writer"], `path "secret/metadata/gsap/*" { capabilities = ["read", "list"] }`) {
		t.Errorf("gsap-writer policy missing metadata path:\n%s", policyDoc["gsap-writer"])
	}
	// SCOPING: gsap's policy must not reach into another team's subtree.
	if strings.Contains(policyDoc["gsap-writer"], "web") {
		t.Errorf("gsap-writer policy leaks into the web subtree:\n%s", policyDoc["gsap-writer"])
	}
}

// TestESOTeamReaderPolicies covers the read half of the team-credentials feature:
// each spec.teams entry gets a read-only `<name>-reader` policy attached to the ESO
// `eso` k8s-auth role, so the openbao ClusterSecretStore can sync team-written app
// secrets. Unlike the writer role, this does NOT gate on a keycloak issuer.
func TestESOTeamReaderPolicies(t *testing.T) {
	teams := []clusterspec.Team{
		{Name: "gsap", OpenbaoSubtree: "secret/gsap"},
		{Name: "web", OpenbaoSubtree: "secret/web"},
	}
	// No keycloak issuer on purpose — the reader wiring must still be emitted.
	steps := baoConfigureSteps("acme/platform", "", teams)

	// role/eso EXACTLY — not role/eso-pusher, which shares the prefix.
	esoRolePolicies := func(all []baoConfigStep) string {
		for _, s := range all {
			if len(s.args) >= 2 && s.args[0] == "write" && s.args[1] == "auth/kubernetes/role/eso" {
				for _, a := range s.args {
					if strings.HasPrefix(a, "policies=") {
						return strings.TrimPrefix(a, "policies=")
					}
				}
			}
		}
		return ""
	}
	esoPolicies := esoRolePolicies(steps)
	readerDoc := map[string]string{}
	for _, s := range steps {
		if len(s.args) >= 3 && s.args[0] == "policy" && s.args[1] == "write" && strings.HasSuffix(s.args[2], "-reader") {
			readerDoc[s.args[2]] = s.stdin
		}
	}

	// The eso role carries platform-ci PLUS one reader per team, in declared order.
	if esoPolicies != "platform-ci,gsap-reader,web-reader" {
		t.Errorf("eso role policies = %q, want platform-ci,gsap-reader,web-reader", esoPolicies)
	}
	// Each reader policy grants READ (not write) on data/* + read,list on metadata/*.
	if !strings.Contains(readerDoc["gsap-reader"], `path "secret/data/gsap/*" { capabilities = ["read"] }`) {
		t.Errorf("gsap-reader missing data read path:\n%s", readerDoc["gsap-reader"])
	}
	if !strings.Contains(readerDoc["gsap-reader"], `path "secret/metadata/gsap/*" { capabilities = ["read", "list"] }`) {
		t.Errorf("gsap-reader missing metadata path:\n%s", readerDoc["gsap-reader"])
	}
	// Read-only: ESO never writes app secrets — no create/update capability.
	if strings.Contains(readerDoc["gsap-reader"], "create") || strings.Contains(readerDoc["gsap-reader"], "update") {
		t.Errorf("gsap-reader must be read-only, got:\n%s", readerDoc["gsap-reader"])
	}
	// SCOPING: each reader POLICY names only its own team's subtree. Note this is
	// not per-team confidentiality at the ExternalSecret layer — every reader is
	// attached to the one shared `eso` identity (see baoConfigureSteps' scope
	// caveat); it asserts the policy generator doesn't over-grant.
	if strings.Contains(readerDoc["gsap-reader"], "web") {
		t.Errorf("gsap-reader leaks into the web subtree:\n%s", readerDoc["gsap-reader"])
	}

	// No teams → eso role is exactly platform-ci and no reader policies emitted, so
	// a team-less instance is byte-identical to before.
	nilSteps := baoConfigureSteps("acme/platform", "", nil)
	for _, s := range nilSteps {
		if len(s.args) >= 3 && s.args[0] == "policy" && s.args[1] == "write" && strings.HasSuffix(s.args[2], "-reader") {
			t.Errorf("team-less instance emitted a reader policy: %s", s.args[2])
		}
	}
	if p := esoRolePolicies(nilSteps); p != "platform-ci" {
		t.Errorf("team-less eso role policies = %q, want platform-ci", p)
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

// configureStub returns a baoread.ExecFn stub with per-command behavior overrides.
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

	if err := RunBaoConfigure(false, "primary"); err != nil {
		t.Fatal(err)
	}
	// lookup + 19 steps (15 base + 4 GitHub-OIDC) + audit list.
	if len(calls) != 21 {
		t.Fatalf("got %d bao calls, want 21: %v", len(calls), calls)
	}
	if calls[0] != "token lookup -format=json" || calls[20] != "audit list" {
		t.Errorf("unexpected first/last calls: %q / %q", calls[0], calls[20])
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
	if err := RunBaoConfigure(false, "primary"); err != nil {
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
	err := RunBaoConfigure(false, "primary")
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
	if err := RunBaoConfigure(false, "primary"); err == nil || !strings.Contains(err.Error(), "preflight") {
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
	if err := RunBaoConfigure(false, "primary"); err == nil || !strings.Contains(err.Error(), "not root") {
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
	if err := RunBaoConfigure(false, "primary"); err != nil {
		t.Fatalf("missing audit device must warn, not fail: %v", err)
	}
	b, _ := os.ReadFile(envFile)
	if string(b) != "BOOTSTRAP_ERRORS=true\n" {
		t.Errorf("GITHUB_ENV = %q, want BOOTSTRAP_ERRORS=true", b)
	}
}

// TestSystemSecretNamespacesCoverPolicyPaths pins the team-subtree security
// boundary: clusterspec.SystemSecretNamespaces (the denylist a team openbaoSubtree
// may not carve into) MUST cover every top segment of the platform OpenBao
// policies here. If a new platform secret namespace is added to a policy without
// adding it to the denylist, a team could scope `secret/<ns>` and self-grant write
// on platform credentials — this test fails the moment that drift is introduced.
func TestSystemSecretNamespacesCoverPolicyPaths(t *testing.T) {
	policies := []string{
		policyPlatformCI, policySecretPropagator, policyESOPusher,
		policyLinodeRotator, policyHarborProvisioner, policyReconcilerRead,
		policyBroadPATRotator,
	}
	// Guard against a NEW `const policy… =` being added without extending the list
	// above (which would leave its paths unchecked).
	src, err := os.ReadFile("openbao_configure.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	// Match DECLARATIONS (`const policyX =`), not literal occurrences, so a comment
	// mentioning "const policy" can't skew the count.
	declRe := regexp.MustCompile(`(?m)^const policy[A-Za-z]+ =`)
	if n := len(declRe.FindAllString(string(src), -1)); n != len(policies) {
		t.Fatalf("found %d `const policy… =` declarations but this test lists %d — add the new policy to `policies` so its secret paths are drift-checked", n, len(policies))
	}
	re := regexp.MustCompile(`secret/(?:data|metadata)/([a-z0-9-]+)/`)
	for _, p := range policies {
		for _, m := range re.FindAllStringSubmatch(p, -1) {
			ns := m[1]
			if !clusterspec.SystemSecretNamespaces[ns] {
				t.Errorf("platform policy path secret/.../%s/ is NOT in clusterspec.SystemSecretNamespaces — a team could claim secret/%s and self-grant platform write; add %q to the denylist in clusterspec/validate.go", ns, ns, ns)
			}
		}
	}
}

// TestDiscoverKeycloakIssuerFromCluster covers the Managed App Platform issuer
// discovery: it reads otomi/otomi-api's SSO_ISSUER (trimmed) and yields "" on
// any kubectl error, so keycloakIssuerFor cleanly falls through when unreachable.
func TestDiscoverKeycloakIssuerFromCluster(t *testing.T) {
	var gotArgs []string
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			t.Errorf("shelled out to %q, want kubectl", name)
		}
		gotArgs = args
		return []byte("https://keycloak.lke634445.akamai-apl.net/realms/otomi\n"), nil
	})
	got := DiscoverIssuerFromCluster()
	if want := "https://keycloak.lke634445.akamai-apl.net/realms/otomi"; got != want {
		t.Errorf("issuer = %q, want %q (trimmed)", got, want)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "-n otomi") || !strings.Contains(joined, "otomi-api") ||
		!strings.Contains(joined, "SSO_ISSUER") {
		t.Errorf("read the wrong object: %v", gotArgs)
	}

	// kubectl error → empty (issuer unresolvable → team steps skipped upstream).
	withExecOutput(t, func(string, ...string) ([]byte, error) { return nil, errors.New("no cluster") })
	if got := DiscoverIssuerFromCluster(); got != "" {
		t.Errorf("on error = %q, want empty", got)
	}
}

// TestManagedDomainFromIssuer covers the pure parse that turns apl-core's Keycloak
// realm issuer into the bare Managed App Platform domain suffix.
func TestManagedDomainFromIssuer(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://keycloak.lke634487.akamai-apl.net/realms/otomi", "lke634487.akamai-apl.net"},
		{"https://keycloak.lke1.akamai-apl.net/realms/otomi/", "lke1.akamai-apl.net"},
		{"https://keycloak.example.com", "example.com"}, // no path is fine
		{"", ""},
		{"https://console.lke1.akamai-apl.net/realms/otomi", ""}, // not the keycloak host
		{"http://keycloak.lke1.akamai-apl.net/realms/otomi", ""}, // must be https
		{"keycloak.lke1.akamai-apl.net", ""},                     // no scheme
	}
	for _, c := range cases {
		if got := ManagedDomainFromIssuer(c.in); got != c.want {
			t.Errorf("ManagedDomainFromIssuer(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestKeycloakAuthConfigDoesNotPinACustomCA pins the fix for the team-write failure
// in e2e run 30510747640. jwks_ca_pem REPLACES the system trust store, so pinning
// apl-core's custom-ca made the JWKS fetch fail with
// "x509: certificate signed by unknown authority" — on managed clusters Linode
// issues the keycloak.<domain> wildcard from a PUBLIC CA, which custom-ca is not in.
//
// The pin looks security-relevant, so without this test the obvious "hardening"
// change silently breaks every team login — and it breaks it LAZILY, at first login
// rather than at configure time (skip_jwks_validation=true), which is why it took a
// full e2e run to surface.
func TestKeycloakAuthConfigDoesNotPinACustomCA(t *testing.T) {
	steps := keycloakTeamSteps("https://keycloak.lke1.akamai-apl.net/realms/otomi",
		[]clusterspec.Team{{Name: "platform", OpenbaoSubtree: "secret/platform"}})
	if len(steps) == 0 {
		t.Fatal("no steps built")
	}
	var cfg []string
	for _, s := range steps {
		if len(s.args) > 1 && s.args[0] == "write" && s.args[1] == "auth/keycloak/config" {
			cfg = s.args
		}
	}
	if cfg == nil {
		t.Fatal("no auth/keycloak/config write step")
	}
	joined := strings.Join(cfg, " ")
	if strings.Contains(joined, "jwks_ca_pem") {
		t.Error("jwks_ca_pem is set again: it REPLACES the system roots, and the managed " +
			"gateway's wildcard is publicly issued — this fails team login with x509: " +
			"certificate signed by unknown authority. See keycloakJWKSTrustNote.")
	}
	// The things that DO have to stay: verified TLS to the public host, and the
	// issuer binding that stops a token from another realm being accepted.
	if !strings.Contains(joined, "jwks_url=https://") {
		t.Error("jwks_url must be https — plaintext allows key substitution outright")
	}
	if !strings.Contains(joined, "bound_issuer=https://keycloak.lke1.akamai-apl.net/realms/otomi") {
		t.Error("bound_issuer must be the public issuer")
	}
	// jwks_url host and bound_issuer host must agree, which is why the URL is derived
	// rather than written twice.
	if !strings.Contains(joined, "jwks_url=https://keycloak.lke1.akamai-apl.net/realms/otomi/") {
		t.Errorf("jwks_url host disagrees with bound_issuer: %s", joined)
	}
}
