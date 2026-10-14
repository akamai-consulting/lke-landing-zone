package assertidentity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

func extSpec() clusterspec.OpenBaoExtensions {
	return clusterspec.OpenBaoExtensions{
		Mounts: []clusterspec.OpenBaoMount{
			{Path: "pki-a", Type: "pki", Operators: []string{"ohttp"},
				Config: []clusterspec.OpenBaoWrite{{Path: "roles/r", Data: map[string]any{"client_flag": true, "ttl": "720h"}}}},
			{Path: "pki-b", Type: "pki"},
		},
		Policies: []clusterspec.OpenBaoPolicy{
			{Name: "p1", Rules: []clusterspec.OpenBaoPolicyRule{
				{Path: "pki-a/issue/r", Capabilities: []string{"create", "update"}},
				{Path: "secret/data/ohttp/+/cert", Capabilities: []string{"read"}},
			}},
			{Name: "p2", Rules: []clusterspec.OpenBaoPolicyRule{
				{Path: "pki-a/issue/r", Capabilities: []string{"read"}},
				{Path: "pki-a/certs/*", Capabilities: []string{"list"}},
			}},
		},
		KubernetesRoles: []clusterspec.OpenBaoKubernetesRole{{
			Name: "k", Policies: []string{"p1", "p2"},
			BoundServiceAccountNames: []string{"sa"}, BoundServiceAccountNamespaces: []string{"ns"},
		}},
	}
}

func TestProbePath(t *testing.T) {
	for in, want := range map[string]string{
		"pki-a/issue/r":            "pki-a/issue/r",
		"pki-a/*":                  "pki-a/_llz_probe",
		"pki-a/certs*":             "pki-a/certs_llz_probe",
		"secret/data/ohttp/+/cert": "secret/data/ohttp/_llz_probe/cert",
	} {
		if got := probePath(in); got != want {
			t.Errorf("probePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Expectations come from the spec — merged across a role's policies, with the
// mounts its rules reach — and nothing else.
func TestRoleExpectationsFromSpec(t *testing.T) {
	es := roleExpectations(extSpec())
	if len(es) != 1 {
		t.Fatalf("got %d expectations", len(es))
	}
	e := es[0]
	if e.Namespace != "ns" || e.ServiceAccount != "sa" || strings.Join(e.Policies, ",") != "p1,p2" {
		t.Fatalf("bound identity wrong: %+v", e)
	}
	if got := strings.Join(e.Caps["pki-a/issue/r"], ","); got != "create,update,read" {
		t.Errorf("caps across policies not merged: %s", got)
	}
	if got := strings.Join(e.Caps["pki-a/certs/_llz_probe"], ","); got != "list" {
		t.Errorf("glob rule probe missing: %v", e.Caps)
	}
	if e.Mounts["pki-a"] != "pki" || len(e.Mounts) != 1 {
		t.Errorf("mounts reached = %v, want only pki-a (secret/ is not a declared mount)", e.Mounts)
	}
}

// The operator expectation is built from the SAME clusterspec functions the
// writer renders — every rule probed for its capabilities, every denied path
// probed for DENY — so the gate cannot drift from the policy bao-configure writes.
func TestOperatorExpectationsCoverRulesAndDenials(t *testing.T) {
	es := operatorExpectations(extSpec(), "ohttp")
	if len(es) != 1 || es[0].Mount != "pki-a" {
		t.Fatalf("got %+v", es)
	}
	mnt := extSpec().Mounts[0]
	rules := clusterspec.OperatorRules(mnt)
	denied := clusterspec.OperatorDeniedPaths(mnt)
	if len(rules) == 0 || len(denied) == 0 {
		t.Fatal("operator rules/denials are empty — the gate would check nothing")
	}
	for _, r := range rules {
		if got := strings.Join(es[0].Caps[probePath(r.Path)], ","); got != strings.Join(r.Capabilities, ",") {
			t.Errorf("rule %s: expectation %q, writer grants %v", r.Path, got, r.Capabilities)
		}
	}
	for _, p := range append([]string{"sys/mounts/pki-a", "pki-a/root/generate/exported", "pki-a/sign-verbatim", "pki-a/root"}, denied...) {
		if got := es[0].Caps[p]; len(got) != 1 || got[0] != "deny" {
			t.Errorf("operators must be checked for DENY on %s, got %v", p, got)
		}
	}
	if len(es[0].Caps) != len(rules)+len(denied) {
		t.Errorf("a denied path collides with a rule probe: %d caps for %d rules + %d denials", len(es[0].Caps), len(rules), len(denied))
	}
	if strings.Join(es[0].ReadOnly, ",") != "pki-a/roles/r" {
		t.Errorf("spec-owned config must be checked read-only, got %v", es[0].ReadOnly)
	}
	if len(operatorExpectations(extSpec(), "someone-else")) != 0 {
		t.Fatal("a team that operates nothing must expect nothing")
	}
}

func TestEvalPolicySet(t *testing.T) {
	if p := evalPolicySet("s", []string{"p1", "p2"}, []string{"default", "p2", "p1"}); len(p) != 0 {
		t.Fatalf("exact set (plus default) must pass: %v", p)
	}
	if p := evalPolicySet("s", []string{"p1"}, []string{"default", "p1", "platform-ci"}); len(p) != 1 || !strings.HasPrefix(p[0], "SECURITY") || !strings.Contains(p[0], "platform-ci") {
		t.Fatalf("an undeclared policy is a SECURITY finding: %v", p)
	}
	if p := evalPolicySet("s", []string{"p1", "p2"}, []string{"default", "p1"}); len(p) != 1 || !strings.Contains(p[0], `lacks declared policy "p2"`) {
		t.Fatalf("a missing declared policy must fail: %v", p)
	}
}

// Two policies on one role that meet on a path: deny wins, as in OpenBao, so the
// gate expects DENY rather than a grant the cluster will (correctly) refuse.
func TestUnionCapsDenyWins(t *testing.T) {
	if got := unionCaps([]string{"read"}, []string{"deny"}); strings.Join(got, ",") != "deny" {
		t.Fatalf("got %v", got)
	}
	if got := unionCaps([]string{"read"}, []string{"list", "read"}); strings.Join(got, ",") != "read,list" {
		t.Fatalf("got %v", got)
	}
}

func TestEvalCapabilities(t *testing.T) {
	want := map[string][]string{"a": {"create", "read"}, "sys/mounts/m": {"deny"}}
	ok := map[string][]string{"a": {"create", "read", "update"}, "sys/mounts/m": {"deny"}}
	if p := evalCapabilities("s", want, ok); len(p) != 0 {
		t.Fatalf("superset must pass: %v", p)
	}
	missing := map[string][]string{"a": {"create"}, "sys/mounts/m": {"deny"}}
	if p := evalCapabilities("s", want, missing); len(p) != 1 || !strings.Contains(p[0], `lacks "read"`) {
		t.Fatalf("missing capability not reported: %v", p)
	}
	leak := map[string][]string{"a": {"create", "read"}, "sys/mounts/m": {"update"}}
	if p := evalCapabilities("s", want, leak); len(p) != 1 || !strings.HasPrefix(p[0], "SECURITY") {
		t.Fatalf("a granted path that must be denied is a SECURITY finding: %v", p)
	}
	unanswered := map[string][]string{"a": {"create", "read"}}
	if p := evalCapabilities("s", want, unanswered); len(p) != 1 || !strings.Contains(p[0], "no answer") {
		t.Fatalf("an unanswered path must fail: %v", p)
	}
	if p := evalCapabilities("s", nil, ok); len(p) != 1 || !strings.Contains(p[0], "nothing to check") {
		t.Fatalf("an empty expectation must fail closed: %v", p)
	}
}

func TestEvalConfigReadback(t *testing.T) {
	declared := map[string]any{"client_flag": true, "ttl": "720h"}
	if p := evalConfigReadback("s", declared, map[string]any{"client_flag": true, "ttl": 2592000}, true); len(p) != 0 {
		t.Fatalf("normalised duration must not fail the read-back: %v", p)
	}
	if p := evalConfigReadback("s", declared, map[string]any{"client_flag": false}, true); len(p) != 1 {
		t.Fatalf("bool mismatch not reported: %v", p)
	}
	if p := evalConfigReadback("s", declared, map[string]any{}, true); len(p) != 1 {
		t.Fatalf("bool absent not reported: %v", p)
	}
	if p := evalConfigReadback("s", declared, nil, false); len(p) != 1 || !strings.Contains(p[0], "absent") {
		t.Fatalf("404 must fail: %v", p)
	}
}

// Read-only means read and nothing that writes — the "at least" check alone
// would pass an operator who can still rewrite a spec-declared role.
func TestEvalReadOnly(t *testing.T) {
	paths := []string{"m/roles/r"}
	if p := evalReadOnly("s", paths, map[string][]string{"m/roles/r": {"read"}}); len(p) != 0 {
		t.Fatalf("read-only must pass: %v", p)
	}
	if p := evalReadOnly("s", paths, map[string][]string{"m/roles/r": {"read", "update"}}); len(p) != 1 || !strings.HasPrefix(p[0], "SECURITY") {
		t.Fatalf("a writable spec-owned path is a SECURITY finding: %v", p)
	}
	if p := evalReadOnly("s", paths, map[string][]string{"m/roles/r": {"deny"}}); len(p) != 2 {
		t.Fatalf("an unreadable path fails (and deny is not read): %v", p)
	}
	if p := evalReadOnly("s", paths, map[string][]string{}); len(p) != 1 || !strings.Contains(p[0], "no answer") {
		t.Fatalf("an unanswered path must fail: %v", p)
	}
}

func TestIssuerWarning(t *testing.T) {
	if w := issuerWarning("m", []string{"id"}); w != "" {
		t.Fatalf("an issuer present must not warn: %q", w)
	}
	if w := issuerWarning("m", nil); !strings.HasPrefix(w, "::warning::") || !strings.Contains(w, "HA pair") {
		t.Fatalf("no issuer must warn, naming the per-cluster step: %q", w)
	}
}

func TestServiceAccountAbsent(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"service account absent": {errors.New(`kubectl create token app/app-sa: error: failed to create token: serviceaccounts "app-sa" not found`), true},
		"namespace absent":       {errors.New(`kubectl create token app/app-sa: Error from server (NotFound): namespaces "app" not found`), true},
		// The two that used to pass as "not deployed yet" and verify nothing:
		"kubectl missing": {errors.New(`kubectl create token app/app-sa: exec: "kubectl": executable file not found in $PATH`), false},
		"API not served":  {errors.New(`kubectl create token app/app-sa: error: the server could not find the requested resource (post serviceaccounts.token) not found`), false},
		"a different SA":  {errors.New(`kubectl create token app/app-sa: serviceaccounts "other" not found`), false},
		"a different ns":  {errors.New(`kubectl create token app/app-sa: namespaces "other" not found`), false},
		"forbidden":       {errors.New("kubectl create token app/app-sa: forbidden"), false},
		"no error":        {nil, false},
	} {
		if got := serviceAccountAbsent(c.err, "app", "app-sa"); got != c.want {
			t.Errorf("%s: absent=%v, want %v", name, got, c.want)
		}
	}
}

func TestRoleSummary(t *testing.T) {
	if s := roleSummary(2, 2); !strings.HasPrefix(s, "✓") {
		t.Fatalf("all verified must be a pass line: %q", s)
	}
	for _, v := range []int{0, 1} {
		if s := roleSummary(v, 2); strings.Contains(s, "✓") || !strings.HasPrefix(s, "::warning::") {
			t.Fatalf("%d of 2 verified must be a warning, never a ✓: %q", v, s)
		}
	}
}

func TestEmptyRoleAndOperatorChecksSkip(t *testing.T) {
	if err := runRoleChecks(context.Background(), "http://unused", clusterspec.OpenBaoExtensions{}, true); err != nil {
		t.Fatalf("no roles must be a clean skip: %v", err)
	}
	if err := runOperatorChecks(context.Background(), "http://unused", "t", "tok", clusterspec.OpenBaoExtensions{}); err != nil {
		t.Fatalf("no operated mount must be a clean skip: %v", err)
	}
}
