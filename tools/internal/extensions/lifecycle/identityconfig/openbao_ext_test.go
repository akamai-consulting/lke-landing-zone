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
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/forge"
)

func extFixture() (clusterspec.OpenBaoExtensions, []clusterspec.Team) {
	teams := []clusterspec.Team{{Name: "ohttp", OpenbaoSubtree: "secret/ohttp"}}
	return clusterspec.OpenBaoExtensions{
		Mounts: []clusterspec.OpenBaoMount{{
			Path: "pki-ohttp-relay", Type: "pki", Description: "relay mTLS", MaxLeaseTTL: "26280h",
			Config: []clusterspec.OpenBaoWrite{
				{Path: "roles/relay-client", Data: map[string]any{"client_flag": true, "ttl": "720h"}},
			},
			Operators: []string{"ohttp"},
		}},
		Policies: []clusterspec.OpenBaoPolicy{{Name: "ohttp-relay-mtls", Rules: []clusterspec.OpenBaoPolicyRule{
			{Path: "pki-ohttp-relay/issue/relay-client", Capabilities: []string{"create", "update"}},
			{Path: "secret/data/ohttp/relay-mtls", Capabilities: []string{"create", "update", "read"}},
		}}},
		KubernetesRoles: []clusterspec.OpenBaoKubernetesRole{{
			Name: "ohttp-relay-mtls", Policies: []string{"ohttp-relay-mtls"},
			BoundServiceAccountNames: []string{"external-secrets"}, BoundServiceAccountNamespaces: []string{"external-secrets"},
		}},
	}, teams
}

func stepByArgs(steps []baoConfigStep, prefix ...string) (baoConfigStep, bool) {
	for _, s := range steps {
		if len(s.args) >= len(prefix) && strings.Join(s.args[:len(prefix)], " ") == strings.Join(prefix, " ") {
			return s, true
		}
	}
	return baoConfigStep{}, false
}

func TestOpenBaoExtensionStepsRenderTheSpec(t *testing.T) {
	ext, _ := extFixture()
	steps := openbaoExtensionSteps(ext)

	enable, ok := stepByArgs(steps, "secrets", "enable", "-path=pki-ohttp-relay")
	if !ok || !enable.fatal || enable.tolerate != "path is already in use" || enable.args[len(enable.args)-1] != "pki" {
		t.Fatalf("enable step wrong (must exist, be fatal except for a re-run's \"already in use\", end in the engine type): %+v", enable)
	}
	tune, ok := stepByArgs(steps, "secrets", "tune")
	if !ok || !tune.fatal || !containsString(tune.args, "-max-lease-ttl=26280h") || tune.args[len(tune.args)-1] != "pki-ohttp-relay/" {
		t.Fatalf("tune step wrong (a re-run must still converge the lease cap, fatally): %+v", tune)
	}
	role, ok := stepByArgs(steps, "write", "pki-ohttp-relay/roles/relay-client", "-")
	if !ok || !role.fatal {
		t.Fatalf("config write missing or non-fatal: %+v", role)
	}
	var roleBody map[string]any
	if err := json.Unmarshal([]byte(role.stdin), &roleBody); err != nil || roleBody["client_flag"] != true || roleBody["ttl"] != "720h" {
		t.Fatalf("config body not the declared data: %q (%v)", role.stdin, err)
	}
	op, ok := stepByArgs(steps, "policy", "write", "pki-ohttp-relay-operator", "-")
	if !ok || op.stdin != renderPolicyHCL(clusterspec.OpenBaoPolicy{Rules: clusterspec.OperatorRules(ext.Mounts[0])}) {
		t.Fatalf("operator policy is not clusterspec.OperatorRules: %+v", op)
	}
	// SCAR: the first cut granted `<mount>/*` — root/generate/exported,
	// sign-verbatim and DELETE <mount>/root with it.
	if strings.Contains(op.stdin, `pki-ohttp-relay/*"`) || strings.Contains(op.stdin, "sys/") {
		t.Fatalf("operator policy grants the whole mount or sys/:\n%s", op.stdin)
	}
	if !strings.Contains(op.stdin, `path "pki-ohttp-relay/roles/relay-client" { capabilities = ["read"] }`) {
		t.Fatalf("the spec-declared role must be read-only to operators:\n%s", op.stdin)
	}
	pol, ok := stepByArgs(steps, "policy", "write", "ohttp-relay-mtls", "-")
	if !ok || !strings.Contains(pol.stdin, `path "secret/data/ohttp/relay-mtls" { capabilities = ["create", "update", "read"] }`) {
		t.Fatalf("extension policy wrong: %+v", pol)
	}
	k8s, ok := stepByArgs(steps, "write", "auth/kubernetes/role/ohttp-relay-mtls", "-")
	if !ok {
		t.Fatal("kubernetes role step missing")
	}
	var body k8sRoleBody
	if err := json.Unmarshal([]byte(k8s.stdin), &body); err != nil {
		t.Fatal(err)
	}
	if strings.Join(body.TokenPolicies, ",") != "ohttp-relay-mtls" || body.TokenTTL != "15m" ||
		body.BoundServiceAccountNamespaces[0] != "external-secrets" {
		t.Fatalf("kubernetes role body wrong: %+v", body)
	}
	if len(openbaoExtensionSteps(clusterspec.OpenBaoExtensions{})) != 0 {
		t.Fatal("empty spec.openbao must add no steps")
	}
}

// Extensions come LAST — after every platform step, CI OIDC (jwt) and human team
// login (keycloak) included — on BOTH exits of baoConfigureSteps. An extension
// step is fatal and its config body free-form, so whatever follows a bad one is
// never written; placed mid-sequence, a typo in a pki role took CI OIDC and
// `llz openbao login --team` down on a fresh bootstrap.
func TestExtensionStepsAreTheTail(t *testing.T) {
	ext, teams := extFixture()
	check := func(t *testing.T, steps []baoConfigStep, wantJWT bool) {
		t.Helper()
		first, sawJWT, sawKeycloak := -1, false, false
		for i, s := range steps {
			isExt := strings.HasSuffix(s.desc, "(spec.openbao)") || strings.HasSuffix(s.desc, "(spec.openbao operators)")
			if isExt && first < 0 {
				first = i
			}
			if !isExt && first >= 0 {
				t.Errorf("platform step %q (at %d) runs after the first spec.openbao step (at %d)", s.desc, i, first)
			}
			joined := strings.Join(s.args, " ")
			sawJWT = sawJWT || strings.Contains(joined, "auth/jwt/role/")
			sawKeycloak = sawKeycloak || strings.Contains(joined, "auth/keycloak/role/")
		}
		// Fail closed on vacuity: the ordering means nothing unless all three ran.
		if first < 0 || !sawKeycloak || sawJWT != wantJWT {
			t.Fatalf("examined the wrong sequence: ext at %d, keycloak=%v, jwt=%v (want %v)", first, sawKeycloak, sawJWT, wantJWT)
		}
	}
	t.Run("forge resolves", func(t *testing.T) {
		check(t, baoConfigureSteps("acme/x", "https://keycloak.example.com/realms/otomi", teams, ext), true)
	})
	t.Run("forge fails", func(t *testing.T) {
		prev := forgeFromEnv
		t.Cleanup(func() { forgeFromEnv = prev })
		forgeFromEnv = func() (forge.Forge, error) { return nil, errors.New("LLZ_FORGE_HOST unset") }
		check(t, baoConfigureSteps("acme/x", "https://keycloak.example.com/realms/otomi", teams, ext), false)
	})
}

func TestVerifyExtensionMounts(t *testing.T) {
	ext, _ := extFixture()
	ok := `{"secret/":{"type":"kv"},"pki-ohttp-relay/":{"type":"pki"}}`
	if err := verifyExtensionMounts(ok, ext); err != nil {
		t.Fatalf("declared mount present: %v", err)
	}
	for name, c := range map[string]struct{ json, want string }{
		"absent":     {`{"secret/":{"type":"kv"}}`, "not mounted"},
		"wrong type": {`{"pki-ohttp-relay/":{"type":"transit"}}`, "is a transit mount"},
		"empty":      {`{}`, "no mounts"},
		"malformed":  {`not json`, "parse"},
	} {
		if err := verifyExtensionMounts(c.json, ext); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", name, c.want, err)
		}
	}
}

// A config write with no data must send an empty JSON object, not `null`.
func TestConfigWriteWithoutDataSendsEmptyObject(t *testing.T) {
	steps := openbaoExtensionSteps(clusterspec.OpenBaoExtensions{Mounts: []clusterspec.OpenBaoMount{{
		Path: "pki-x", Type: "pki", Config: []clusterspec.OpenBaoWrite{{Path: "roles/r"}},
	}}})
	w, ok := stepByArgs(steps, "write", "pki-x/roles/r", "-")
	if !ok || w.stdin != "{}" {
		t.Fatalf("got %+v", w)
	}
}

// The team that operates a mount must receive the operator policy on its
// keycloak role — that is the whole non-root path. Uses the SAME name function
// the live gate expects (clusterspec.OperatorPolicyName).
func TestOperatorTeamRoleCarriesTheOperatorPolicy(t *testing.T) {
	ext, teams := extFixture()
	teams = append(teams, clusterspec.Team{Name: "other", OpenbaoSubtree: "secret/other"})
	steps := keycloakTeamSteps("https://keycloak.example.com/realms/otomi", teams, ext)
	policiesFor := func(team string) []string {
		s, ok := stepByArgs(steps, "write", "auth/keycloak/role/"+team, "-")
		if !ok {
			t.Fatalf("no keycloak role for %s", team)
		}
		var b keycloakRoleBody
		if err := json.Unmarshal([]byte(s.stdin), &b); err != nil {
			t.Fatal(err)
		}
		return b.TokenPolicies
	}
	want := clusterspec.OperatorPolicyName("pki-ohttp-relay")
	if got := policiesFor("ohttp"); strings.Join(got, ",") != "ohttp-writer,"+want {
		t.Fatalf("operator team policies = %v, want [ohttp-writer %s]", got, want)
	}
	if got := policiesFor("other"); strings.Join(got, ",") != "other-writer" {
		t.Fatalf("non-operator team gained policies: %v", got)
	}
	// And the policy the role names is actually written in the same run.
	all := baoConfigureSteps("acme/x", "https://keycloak.example.com/realms/otomi", teams, ext)
	if _, ok := stepByArgs(all, "policy", "write", want, "-"); !ok {
		t.Fatalf("team role names %s but no step writes it", want)
	}
}

// The reserved name sets in clusterspec are what stop an extension from reusing
// a platform policy or role. They are checked against the REAL configure
// sequence — every policy and kubernetes role bao-configure writes on its own must
// be reserved — so adding a platform policy without reserving it fails here.
func TestReservedOpenBaoNamesCoverConfigureSteps(t *testing.T) {
	teams := []clusterspec.Team{{Name: "t1", OpenbaoSubtree: "secret/t1"}}
	steps := baoConfigureSteps("acme/x", "https://keycloak.example.com/realms/otomi", teams, clusterspec.OpenBaoExtensions{})
	var sawPolicy, sawRole, sawMount int
	for _, s := range steps {
		a := s.args
		switch {
		case len(a) >= 3 && a[0] == "policy" && a[1] == "write":
			sawPolicy++
			name := a[2]
			if strings.HasSuffix(name, "-reader") || strings.HasSuffix(name, "-writer") {
				continue // refused by suffix
			}
			if !clusterspec.ReservedOpenBaoPolicies[name] {
				t.Errorf("bao-configure writes policy %q but clusterspec.ReservedOpenBaoPolicies does not reserve it — an extension could overwrite it", name)
			}
		case len(a) >= 2 && a[0] == "write" && strings.HasPrefix(a[1], "auth/kubernetes/role/"):
			sawRole++
			if name := strings.TrimPrefix(a[1], "auth/kubernetes/role/"); !clusterspec.ReservedOpenBaoKubernetesRoles[name] {
				t.Errorf("bao-configure writes kubernetes role %q but it is not reserved", name)
			}
		case len(a) >= 3 && a[0] == "secrets" && a[1] == "enable":
			sawMount++
			for _, f := range a {
				if p, ok := strings.CutPrefix(f, "-path="); ok && !clusterspec.ReservedOpenBaoMounts[p] {
					t.Errorf("bao-configure enables mount %q but it is not reserved", p)
				}
			}
		}
	}
	// Fail closed on vacuity: a refactor that renamed the argv shape would make
	// every branch above miss and the test pass having checked nothing.
	if sawPolicy < 5 || sawRole < 3 || sawMount < 1 {
		t.Fatalf("examined too little (policies=%d roles=%d mounts=%d) — did the step argv shape change?", sawPolicy, sawRole, sawMount)
	}
}

// renderPolicyHCL must render exactly the declared rules — the live gate checks
// those rules (not the HCL) against sys/capabilities-self, so a renderer that
// dropped or reordered a capability would pass review and fail only on a cluster.
func TestExtensionPolicyHCLMatchesRules(t *testing.T) {
	ext, _ := extFixture()
	p := ext.Policies[0]
	line := regexp.MustCompile(`^path "([^"]+)" \{ capabilities = \[(.*)\] \}$`)
	lines := strings.Split(strings.TrimSpace(renderPolicyHCL(p)), "\n")
	if len(lines) != len(p.Rules) {
		t.Fatalf("rendered %d stanzas for %d rules", len(lines), len(p.Rules))
	}
	for i, l := range lines {
		m := line.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("unparseable stanza %q", l)
		}
		var caps []string
		for _, c := range strings.Split(m[2], ", ") {
			caps = append(caps, strings.Trim(c, `"`))
		}
		if m[1] != p.Rules[i].Path || strings.Join(caps, ",") != strings.Join(p.Rules[i].Capabilities, ",") {
			t.Errorf("stanza %d = %s %v, rule = %s %v", i, m[1], caps, p.Rules[i].Path, p.Rules[i].Capabilities)
		}
	}
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// An unreadable spec skips spec.openbao only when it declares none — a
// commented-out example is not a declaration.
func TestDeclaresOpenBao(t *testing.T) {
	for raw, want := range map[string]bool{
		"spec:\n  openbao:\n    mounts: []\n": true,
		"spec:\n  openbao :\n":                true,
		"spec:\n  # openbao:\n  teams: []\n":  false,
		"spec:\n  teams: []\n":                false,
		"spec:\n  openbaoX: 1\n":              false,
	} {
		if got := declaresOpenBao([]byte(raw)); got != want {
			t.Errorf("declaresOpenBao(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestSpecOpenBaoFailsOnlyWhenAnUnreadableSpecDeclaresIt(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if ext, err := SpecOpenBao(); err != nil || !ext.Empty() {
		t.Fatalf("no spec: want empty, got %+v, %v", ext, err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, clusterspec.LandingZoneFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("apiVersion: x\nkind: LandingZone\nspec:\n  bogusField: 1\n")
	if ext, err := SpecOpenBao(); err != nil || !ext.Empty() {
		t.Fatalf("unreadable spec without openbao: want empty (platform only), got %+v, %v", ext, err)
	}
	write("apiVersion: x\nkind: LandingZone\nspec:\n  openbao:\n    mountz: []\n")
	if _, err := SpecOpenBao(); err == nil || !strings.Contains(err.Error(), "cannot be skipped") {
		t.Fatalf("unreadable spec WITH openbao must fail, got %v", err)
	}
}
