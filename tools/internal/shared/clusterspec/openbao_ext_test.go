package clusterspec

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// pkiExample is the motivating spec: an application-owned PKI issuing tier whose
// leaves ESO issues and hands off through the team's KV subtree.
func pkiExample() (OpenBaoExtensions, []Team) {
	teams := []Team{{Name: "ohttp", OpenbaoSubtree: "secret/ohttp"}}
	return OpenBaoExtensions{
		Mounts: []OpenBaoMount{{
			Path: "pki-ohttp-relay", Type: "pki", MaxLeaseTTL: "26280h",
			Config: []OpenBaoWrite{
				{Path: "roles/relay-client", Data: map[string]any{"client_flag": true, "server_flag": false}},
				{Path: "config/crl", Data: map[string]any{"expiry": "72h"}},
				{Path: "config/auto-tidy", Data: map[string]any{"enabled": true}},
			},
			Operators: []string{"ohttp"},
		}},
		Policies: []OpenBaoPolicy{{Name: "ohttp-relay-mtls", Rules: []OpenBaoPolicyRule{
			{Path: "pki-ohttp-relay/issue/relay-client", Capabilities: []string{"create", "update"}},
			{Path: "secret/data/ohttp/relay-mtls", Capabilities: []string{"create", "update", "read"}},
			{Path: "secret/metadata/ohttp/relay-mtls", Capabilities: []string{"create", "update", "read"}},
		}}},
		KubernetesRoles: []OpenBaoKubernetesRole{{
			Name: "ohttp-relay-mtls", Policies: []string{"ohttp-relay-mtls"},
			BoundServiceAccountNames: []string{"external-secrets"}, BoundServiceAccountNamespaces: []string{"external-secrets"},
		}},
	}, teams
}

func TestValidateOpenBaoAcceptsThePKIExample(t *testing.T) {
	ext, teams := pkiExample()
	if errs := ValidateOpenBao(ext, teams); len(errs) != 0 {
		t.Fatalf("valid example rejected: %v", errs)
	}
	if errs := ValidateOpenBao(OpenBaoExtensions{}, nil); len(errs) != 0 {
		t.Fatalf("empty spec.openbao rejected: %v", errs)
	}
}

// Each mutation is something an extension must never be able to do. Every case
// names the substring its error must carry, so a case cannot pass by tripping an
// unrelated check.
func TestValidateOpenBaoRefusesEscalations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*OpenBaoExtensions)
		want   string
	}{
		{"reserved mount", func(e *OpenBaoExtensions) { e.Mounts[0].Path = "secret" }, "reserved"},
		{"auth mount name", func(e *OpenBaoExtensions) { e.Mounts[0].Path = "keycloak" }, "reserved"},
		{"slash in mount", func(e *OpenBaoExtensions) { e.Mounts[0].Path = "pki/x" }, "kebab"},
		{"unsupported engine", func(e *OpenBaoExtensions) { e.Mounts[0].Type = "kv" }, "unsupported"},
		{"bad lease ttl", func(e *OpenBaoExtensions) { e.Mounts[0].MaxLeaseTTL = "3y" }, "not a duration"},
		{"root generation via config", func(e *OpenBaoExtensions) {
			e.Mounts[0].Config = append(e.Mounts[0].Config, OpenBaoWrite{Path: "root/generate/exported"})
		}, "not an allowed pki configuration endpoint"},
		{"intermediate generation via config", func(e *OpenBaoExtensions) {
			e.Mounts[0].Config = append(e.Mounts[0].Config, OpenBaoWrite{Path: "intermediate/generate/internal"})
		}, "not an allowed pki configuration endpoint"},
		{"issuing via config", func(e *OpenBaoExtensions) {
			e.Mounts[0].Config = append(e.Mounts[0].Config, OpenBaoWrite{Path: "issue/relay-client"})
		}, "not an allowed pki configuration endpoint"},
		{"traversal in config", func(e *OpenBaoExtensions) {
			e.Mounts[0].Config = append(e.Mounts[0].Config, OpenBaoWrite{Path: "roles/../../sys/mounts"})
		}, "not an allowed"},
		{"transit engine", func(e *OpenBaoExtensions) {
			e.Mounts = append(e.Mounts, OpenBaoMount{Path: "transit-x", Type: "transit"})
		}, "unsupported"},
		{"acme via config", func(e *OpenBaoExtensions) {
			e.Mounts[0].Config = append(e.Mounts[0].Config, OpenBaoWrite{Path: "config/acme", Data: map[string]any{"enabled": true}})
		}, "not an allowed pki configuration endpoint"},
		{"ca import via config", func(e *OpenBaoExtensions) {
			e.Mounts[0].Config = append(e.Mounts[0].Config, OpenBaoWrite{Path: "config/ca"})
		}, "not an allowed pki configuration endpoint"},
		{"deny mixed with grants", func(e *OpenBaoExtensions) {
			e.Policies[0].Rules[0].Capabilities = []string{"read", "deny"}
		}, "mixes deny"},
		{"operator not a team", func(e *OpenBaoExtensions) { e.Mounts[0].Operators = []string{"nobody"} }, "not a spec.teams name"},
		{"platform policy name", func(e *OpenBaoExtensions) { e.Policies[0].Name = "eso-pusher" }, "reserved"},
		{"team writer name", func(e *OpenBaoExtensions) { e.Policies[0].Name = "ohttp-writer" }, "reserved"},
		{"operator policy name", func(e *OpenBaoExtensions) { e.Policies[0].Name = "pki-ohttp-relay-operator" }, "reserved"},
		{"platform KV path", func(e *OpenBaoExtensions) {
			e.Policies[0].Rules[0].Path = "secret/data/linode/api-token"
		}, "outside every declared mount"},
		{"sys path", func(e *OpenBaoExtensions) { e.Policies[0].Rules[0].Path = "sys/mounts/pki-ohttp-relay" }, "outside every declared mount"},
		{"auth path", func(e *OpenBaoExtensions) { e.Policies[0].Rules[0].Path = "auth/kubernetes/role/eso" }, "outside every declared mount"},
		{"undeclared mount", func(e *OpenBaoExtensions) { e.Policies[0].Rules[0].Path = "pki-other/issue/x" }, "outside every declared mount"},
		{"bare mount glob", func(e *OpenBaoExtensions) { e.Policies[0].Rules[0].Path = "pki-ohttp-relay*" }, "outside every declared mount"},
		{"sibling team subtree", func(e *OpenBaoExtensions) {
			e.Policies[0].Rules[0].Path = "secret/data/ohttpx/key"
		}, "outside every declared mount"},
		{"sudo", func(e *OpenBaoExtensions) { e.Policies[0].Rules[0].Capabilities = []string{"sudo"} }, "not allowed"},
		{"no capabilities", func(e *OpenBaoExtensions) { e.Policies[0].Rules[0].Capabilities = nil }, "no capabilities"},
		{"platform role name", func(e *OpenBaoExtensions) { e.KubernetesRoles[0].Name = "eso" }, "reserved"},
		{"role carries platform policy", func(e *OpenBaoExtensions) {
			e.KubernetesRoles[0].Policies = []string{"platform-ci"}
		}, "not a spec.openbao.policies entry"},
		{"role carries team writer", func(e *OpenBaoExtensions) {
			e.KubernetesRoles[0].Policies = []string{"ohttp-writer"}
		}, "not a spec.openbao.policies entry"},
		{"wildcard service account", func(e *OpenBaoExtensions) {
			e.KubernetesRoles[0].BoundServiceAccountNames = []string{"*"}
		}, "wildcards"},
		{"role ttl too long", func(e *OpenBaoExtensions) { e.KubernetesRoles[0].TTL = "24h" }, "(0, 1h]"},
		{"duplicate policy", func(e *OpenBaoExtensions) { e.Policies = append(e.Policies, e.Policies[0]) }, "declared twice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ext, teams := pkiExample()
			c.mutate(&ext)
			errs := ValidateOpenBao(ext, teams)
			for _, err := range errs {
				if strings.Contains(err.Error(), c.want) {
					return
				}
			}
			t.Fatalf("want an error containing %q, got %v", c.want, errs)
		})
	}
}

// The whole-spec validator must run the extension checks: a render that skips
// them would hand bao-configure an unvalidated block (bao-configure re-validates,
// but render is where the operator expects to hear about it).
func TestLandingZoneValidateRunsOpenBaoChecks(t *testing.T) {
	ext, teams := pkiExample()
	ext.Policies[0].Rules[0].Path = "sys/policies/acl/root"
	lz := &LandingZone{Spec: Spec{Teams: teams, OpenBao: ext}}
	for _, err := range lz.Validate() {
		if strings.Contains(err.Error(), "openbao.policies[0].rules[0]") {
			return
		}
	}
	t.Fatal("LandingZone.Validate did not report the spec.openbao violation")
}

// The spec decodes strictly, so the field names here ARE the authoring contract.
func TestOpenBaoBlockDecodes(t *testing.T) {
	lz, err := Decode([]byte(`
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: {name: x}
spec:
  instance: {upstreamOrg: o, repo: o/x, forge: github}
  teams: [{name: ohttp, openbaoSubtree: secret/ohttp}]
  openbao:
    mounts:
      - path: pki-ohttp-relay
        type: pki
        maxLeaseTTL: 26280h
        config:
          - path: roles/relay-client
            data: {client_flag: true, ttl: 720h}
        operators: [ohttp]
    policies:
      - name: ohttp-relay-mtls
        rules:
          - {path: pki-ohttp-relay/issue/relay-client, capabilities: [create, update]}
    kubernetesRoles:
      - name: ohttp-relay-mtls
        boundServiceAccountNames: [external-secrets]
        boundServiceAccountNamespaces: [external-secrets]
        policies: [ohttp-relay-mtls]
        ttl: 15m
`))
	if err != nil {
		t.Fatal(err)
	}
	o := lz.Spec.OpenBao
	if len(o.Mounts) != 1 || o.Mounts[0].Config[0].Data["client_flag"] != true || o.Mounts[0].Operators[0] != "ohttp" {
		t.Fatalf("mounts decoded wrong: %+v", o.Mounts)
	}
	if len(o.Policies) != 1 || o.Policies[0].Rules[0].Capabilities[1] != "update" {
		t.Fatalf("policies decoded wrong: %+v", o.Policies)
	}
	if len(o.KubernetesRoles) != 1 || o.KubernetesRoles[0].EffectiveTTL() != "15m" {
		t.Fatalf("roles decoded wrong: %+v", o.KubernetesRoles)
	}
	if errs := ValidateOpenBao(o, lz.Spec.Teams); len(errs) != 0 {
		t.Fatalf("decoded example invalid: %v", errs)
	}
}

func TestOperatorMountsFor(t *testing.T) {
	ext := OpenBaoExtensions{Mounts: []OpenBaoMount{
		{Path: "b", Operators: []string{"t1"}}, {Path: "a", Operators: []string{"t1", "t2"}}, {Path: "c"},
	}}
	if got := strings.Join(ext.OperatorMountsFor("t1"), ","); got != "a,b" {
		t.Fatalf("t1 operates %q, want a,b", got)
	}
	if got := ext.OperatorMountsFor("t3"); len(got) != 0 {
		t.Fatalf("t3 operates %v, want none", got)
	}
}

// The e2e lane applies .github/e2e/spec-openbao.yaml to a scaffold whose only
// team is DefaultTeamName. If the fixture stops validating — or stops naming that
// team as an operator — the live check would either fail the spec render or skip
// the operator half silently; both are caught here, at PR time.
func TestE2EFixtureValidatesAgainstTheScaffoldTeam(t *testing.T) {
	b, err := os.ReadFile("../../../../.github/e2e/spec-openbao.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var ext OpenBaoExtensions
	if err := yaml.UnmarshalStrict(b, &ext); err != nil {
		t.Fatalf("fixture does not decode strictly: %v", err)
	}
	teams := []Team{{Name: DefaultTeamName, OpenbaoSubtree: "secret/" + DefaultTeamName}}
	if errs := ValidateOpenBao(ext, teams); len(errs) != 0 {
		t.Fatalf("fixture invalid against the scaffold team: %v", errs)
	}
	if len(ext.OperatorMountsFor(DefaultTeamName)) == 0 || len(ext.KubernetesRoles) == 0 || len(ext.Mounts[0].Config) == 0 {
		t.Fatal("fixture must exercise an operator mount, a kubernetes role and a config write — the lane checks all three")
	}
}

// The operator allowlist must grant NONE of the paths the gate requires DENY on —
// held here, on the real functions both sides use, rather than discovered as a
// red e2e lane. Also: every rule is something the spec validator would accept
// (canonical path, allowed capabilities, inside the mount), so the operator
// policy is never broader than an instance could have declared by hand.
func TestOperatorRulesGrantNoDeniedPath(t *testing.T) {
	mnt := OpenBaoMount{Path: "pki-x", Type: "pki", Config: []OpenBaoWrite{
		{Path: "roles/app"}, {Path: "config/crl"},
	}}
	rules, denied := OperatorRules(mnt), OperatorDeniedPaths(mnt)
	if len(rules) < 5 || len(denied) < 5 {
		t.Fatalf("examined too little (rules=%d denied=%d)", len(rules), len(denied))
	}
	for _, d := range denied {
		for _, r := range rules {
			if PolicyPathMatches(r.Path, d) {
				t.Errorf("operator rule %s grants %v on %s, which operators must be DENIED", r.Path, r.Capabilities, d)
			}
		}
	}
	if !PolicyPathMatches("pki-x/roles/+", "pki-x/roles/r") || PolicyPathMatches("pki-x/issuer/+", "pki-x/issuer/i/sign-verbatim") {
		t.Fatal("PolicyPathMatches is wrong — this test would prove nothing")
	}
	mounts := map[string]OpenBaoMount{"pki-x": {Path: "pki-x", Type: "pki"}}
	for _, r := range rules {
		if !rulePathRe.MatchString(r.Path) || !ruleInScope(r.Path, mounts, nil) {
			t.Errorf("operator rule %s is not a path the spec validator would accept", r.Path)
		}
		for _, c := range r.Capabilities {
			if !allowedCaps[c] || c == "deny" {
				t.Errorf("operator rule %s carries capability %q", r.Path, c)
			}
		}
	}
	if OperatorRules(OpenBaoMount{Path: "t", Type: "transit"}) != nil || OperatorDeniedPaths(OpenBaoMount{Path: "t", Type: "transit"}) != nil {
		t.Fatal("only pki has operators")
	}
}

// Spec-owned config is read-only to operators: each declared path has exactly one
// stanza, an exact `[read]` — which outranks `roles/+` in OpenBao — so an
// operator cannot widen a declared role the workload issues from.
func TestOperatorRulesMakeSpecOwnedConfigReadOnly(t *testing.T) {
	mnt := OpenBaoMount{Path: "pki-x", Type: "pki", Config: []OpenBaoWrite{
		{Path: "roles/app"}, {Path: "config/crl"},
	}}
	rules := OperatorRules(mnt)
	owned := SpecOwnedPaths(mnt)
	if strings.Join(owned, ",") != "pki-x/roles/app,pki-x/config/crl" {
		t.Fatalf("spec-owned paths = %v", owned)
	}
	for _, p := range owned {
		n := 0
		for _, r := range rules {
			if r.Path == p {
				n++
				if strings.Join(r.Capabilities, ",") != "read" {
					t.Errorf("%s: operators hold %v, want [read]", p, r.Capabilities)
				}
			}
		}
		if n != 1 {
			t.Errorf("%s: %d stanzas, want exactly one exact rule", p, n)
		}
	}
	// A config path the spec does NOT declare stays operator-writable.
	for _, r := range rules {
		if r.Path == "pki-x/config/urls" && !strings.Contains(strings.Join(r.Capabilities, ","), "update") {
			t.Errorf("undeclared config/urls lost operator write: %v", r.Capabilities)
		}
	}
}

func TestPolicyPathMatches(t *testing.T) {
	for _, c := range []struct {
		rule, path string
		want       bool
	}{
		{"pki/issue/r", "pki/issue/r", true},
		{"pki/issue/r", "pki/issue/r2", false},
		{"pki/issue/+", "pki/issue/r", true},
		{"pki/issue/+", "pki/issue/r/x", false},
		{"pki/*", "pki/root", true},
		{"pki/*", "pki/issuer/i/sign-verbatim", true},
		{"pki/root*", "pki/root/generate/exported", true},
		{"pki/root*", "pki/roles/r", false},
		{"pki/+/generate/*", "pki/root/generate/exported", true},
		{"pki/+/generate/*", "pki/root", false},
		{"pki/sign-verbatim", "pki/sign-verbatim/r", false},
		{"pki*", "pki-other/root", true},
	} {
		if got := PolicyPathMatches(c.rule, c.path); got != c.want {
			t.Errorf("PolicyPathMatches(%q, %q) = %v, want %v", c.rule, c.path, got, c.want)
		}
	}
}

// A workload may WRITE only issuance paths under its mount; reads and lists are
// unrestricted. Every refused path below validated under the earlier denylist
// or before it — roles/evil + issue/evil is sign-verbatim by another name.
func TestValidateOpenBaoWorkloadWritesAreIssuanceOnly(t *testing.T) {
	refused := []string{
		"pki-ohttp-relay/*", "pki-ohttp-relay/root*", "pki-ohttp-relay/root/generate/exported",
		"pki-ohttp-relay/+/generate/*", "pki-ohttp-relay/sign-verbatim", "pki-ohttp-relay/sign-verbatim/+",
		"pki-ohttp-relay/issuer/+/sign-intermediate", "pki-ohttp-relay/issuers/generate/intermediate/exported",
		"pki-ohttp-relay/config/ca", "pki-ohttp-relay/keys/import", "pki-ohttp-relay/issuers/import/bundle",
		"pki-ohttp-relay/roles/relay-client", "pki-ohttp-relay/roles/+", "pki-ohttp-relay/config/crl",
		"pki-ohttp-relay/roles/evil", "pki-ohttp-relay/issuer/+", "pki-ohttp-relay/key/+",
		"pki-ohttp-relay/intermediate/set-signed", "pki-ohttp-relay/intermediate/generate/internal",
		"pki-ohttp-relay/config/issuers", "pki-ohttp-relay/config/keys", "pki-ohttp-relay/tidy",
		"pki-ohttp-relay/issuers/import/cert", "pki-ohttp-relay/revoke", "pki-ohttp-relay/issue/*",
		"pki-ohttp-relay/issue/relay-client/x",
	}
	for _, path := range refused {
		ext, teams := pkiExample()
		ext.Policies[0].Rules[0] = OpenBaoPolicyRule{Path: path, Capabilities: []string{"update"}}
		var hit bool
		for _, err := range ValidateOpenBao(ext, teams) {
			hit = hit || strings.Contains(err.Error(), "outside issuance")
		}
		if !hit {
			t.Errorf("workload write on %s was accepted", path)
		}
	}
	for _, rule := range []OpenBaoPolicyRule{
		{Path: "pki-ohttp-relay/issue/relay-client", Capabilities: []string{"create", "update"}},
		{Path: "pki-ohttp-relay/issue/+", Capabilities: []string{"update"}},
		{Path: "pki-ohttp-relay/sign/relay-client", Capabilities: []string{"update"}},
		{Path: "pki-ohttp-relay/issuer/+/issue/relay-client", Capabilities: []string{"update"}},
		{Path: "pki-ohttp-relay/issuer/default/sign/relay-client", Capabilities: []string{"update"}},
		{Path: "pki-ohttp-relay/*", Capabilities: []string{"read", "list"}},
		{Path: "pki-ohttp-relay/root/generate/exported", Capabilities: []string{"deny"}},
		{Path: "secret/data/ohttp/relay-mtls", Capabilities: []string{"create", "update", "delete"}},
	} {
		ext, teams := pkiExample()
		ext.Policies[0].Rules[0] = rule
		if errs := ValidateOpenBao(ext, teams); len(errs) != 0 {
			t.Errorf("legitimate workload rule %s %v refused: %v", rule.Path, rule.Capabilities, errs)
		}
	}
}
