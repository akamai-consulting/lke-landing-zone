package main

import (
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestParseApproleSecretID(t *testing.T) {
	if got := parseApproleSecretID(`{"data":{"secret_id":"sid-123","secret_id_accessor":"acc"}}`); got != "sid-123" {
		t.Errorf("parseApproleSecretID = %q, want sid-123", got)
	}
	for _, bad := range []string{"", "not json", `{"data":{}}`} {
		if got := parseApproleSecretID(bad); got != "" {
			t.Errorf("parseApproleSecretID(%q) = %q, want empty", bad, got)
		}
	}
}

func TestChooseApproleGHSecret(t *testing.T) {
	cases := []struct {
		base, standby, haRole, want string
	}{
		{"OPENBAO_APPROLE_SECRET_ID", "OPENBAO_APPROLE_SECRET_ID_STANDBY", "active", "OPENBAO_APPROLE_SECRET_ID"},
		{"OPENBAO_APPROLE_SECRET_ID", "OPENBAO_APPROLE_SECRET_ID_STANDBY", "standalone", "OPENBAO_APPROLE_SECRET_ID"},
		{"OPENBAO_APPROLE_SECRET_ID", "OPENBAO_APPROLE_SECRET_ID_STANDBY", "standby", "OPENBAO_APPROLE_SECRET_ID_STANDBY"},
		{"OPENBAO_APPROLE_SECRET_ID", "", "standby", "OPENBAO_APPROLE_SECRET_ID"}, // no standby name configured
		{"", "", "active", ""}, // no gh-secret at all (k8s-only callers)
	}
	for _, tc := range cases {
		if got := chooseApproleGHSecret(tc.base, tc.standby, tc.haRole); got != tc.want {
			t.Errorf("chooseApproleGHSecret(%q,%q,%q) = %q, want %q",
				tc.base, tc.standby, tc.haRole, got, tc.want)
		}
	}
}

func TestParseK8sSecretRef(t *testing.T) {
	ns, name, key, err := parseK8sSecretRef("llz-external-secrets/eso-approle-secret:secretId")
	if err != nil || ns != "llz-external-secrets" || name != "eso-approle-secret" || key != "secretId" {
		t.Errorf("parseK8sSecretRef = (%q,%q,%q,%v)", ns, name, key, err)
	}
	for _, bad := range []string{"", "ns/name", "ns:key", "/name:key", "ns/:key", "ns/name:"} {
		if _, _, _, err := parseK8sSecretRef(bad); err == nil {
			t.Errorf("parseK8sSecretRef(%q) must error", bad)
		}
	}
}

func TestGenericSecretManifest(t *testing.T) {
	m := genericSecretManifest("ns1", "sec1", "secretId", "v@lue:with\nnewline")
	for _, want := range []string{
		"kind: Secret",
		"name: sec1",
		"namespace: ns1",
		"type: Opaque",
		"secretId: " + base64.StdEncoding.EncodeToString([]byte("v@lue:with\nnewline")),
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q:\n%s", want, m)
		}
	}
}

// stubApproleSeams stubs the mint exec + capture sinks for the gh/kubectl
// fan-out. Returns recorders for applied manifests and gh secret writes
// ("repo:NAME=v" / "env:ENV:NAME=v").
func stubApproleSeams(t *testing.T, secretID string) (*[]string, *[]string) {
	t.Helper()
	prevExec, prevApply, prevRepo, prevEnv := baoExecFn, kubectlApplyFn, ghSetRepoSecretFn, ghSetSecretFn
	var manifests, ghWrites []string
	baoExecFn = func(_, token, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		if token == "" || !strings.HasPrefix(joined, "write -f auth/approle/role/") {
			return "", "unexpected: " + joined, errors.New("unexpected")
		}
		return `{"data":{"secret_id":"` + secretID + `"}}`, "", nil
	}
	kubectlApplyFn = func(m string) error { manifests = append(manifests, m); return nil }
	ghSetRepoSecretFn = func(name, value string) error {
		ghWrites = append(ghWrites, "repo:"+name+"="+value)
		return nil
	}
	ghSetSecretFn = func(name, ghEnv, value string) error {
		ghWrites = append(ghWrites, "env:"+ghEnv+":"+name+"="+value)
		return nil
	}
	t.Cleanup(func() {
		baoExecFn, kubectlApplyFn, ghSetRepoSecretFn, ghSetSecretFn = prevExec, prevApply, prevRepo, prevEnv
	})
	return &manifests, &ghWrites
}

func TestRunCISeedApproleESOVariant(t *testing.T) {
	manifests, ghWrites := stubApproleSeams(t, "sid-eso")
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("HA_ROLE", "active")
	opts := seedApproleOpts{
		role:            "platform-ci",
		k8sSecret:       "llz-external-secrets/eso-approle-secret:secretId",
		ghRoleSecret:    "OPENBAO_APPROLE_ROLE_ID",
		ghSecret:        "OPENBAO_APPROLE_SECRET_ID",
		ghSecretStandby: "OPENBAO_APPROLE_SECRET_ID_STANDBY",
	}
	if err := runCISeedApprole(opts); err != nil {
		t.Fatal(err)
	}
	if len(*manifests) != 1 || !strings.Contains((*manifests)[0],
		"secretId: "+base64.StdEncoding.EncodeToString([]byte("sid-eso"))) {
		t.Errorf("eso-approle-secret manifest wrong: %v", *manifests)
	}
	want := []string{
		"repo:OPENBAO_APPROLE_ROLE_ID=platform-ci",
		"repo:OPENBAO_APPROLE_SECRET_ID=sid-eso",
	}
	if strings.Join(*ghWrites, "|") != strings.Join(want, "|") {
		t.Errorf("gh writes = %v, want %v", *ghWrites, want)
	}

	// Same flags on the standby peer route the secret-id to the _STANDBY name.
	*ghWrites = (*ghWrites)[:0]
	t.Setenv("HA_ROLE", "standby")
	if err := runCISeedApprole(opts); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"repo:OPENBAO_APPROLE_ROLE_ID=platform-ci",
		"repo:OPENBAO_APPROLE_SECRET_ID_STANDBY=sid-eso",
	}
	if strings.Join(*ghWrites, "|") != strings.Join(want, "|") {
		t.Errorf("standby gh writes = %v, want %v", *ghWrites, want)
	}
}

func TestRunCISeedApprolePropagatorVariant(t *testing.T) {
	manifests, ghWrites := stubApproleSeams(t, "sid-prop")
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	t.Setenv("HA_ROLE", "active")
	sum := withGHASummaryFile(t)
	err := runCISeedApprole(seedApproleOpts{
		role:         "secret-propagator",
		ghEnv:        "infra-primary",
		ghRoleSecret: "OPENBAO_PROPAGATOR_ROLE_ID",
		ghSecret:     "OPENBAO_PROPAGATOR_SECRET_ID",
		summary:      []string{"", "**secret-propagator AppRole seeded** to `infra-primary`:"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*manifests) != 0 {
		t.Errorf("propagator variant must not touch kubectl, got %v", *manifests)
	}
	want := []string{
		"env:infra-primary:OPENBAO_PROPAGATOR_ROLE_ID=secret-propagator",
		"env:infra-primary:OPENBAO_PROPAGATOR_SECRET_ID=sid-prop",
	}
	if strings.Join(*ghWrites, "|") != strings.Join(want, "|") {
		t.Errorf("gh writes = %v, want %v", *ghWrites, want)
	}
	b, _ := os.ReadFile(sum)
	if !strings.Contains(string(b), "**secret-propagator AppRole seeded** to `infra-primary`:") {
		t.Errorf("summary missing seeded block: %q", b)
	}
}

func TestRunCISeedApproleValidation(t *testing.T) {
	stubApproleSeams(t, "sid")
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	if err := runCISeedApprole(seedApproleOpts{}); err == nil {
		t.Error("missing --role must error")
	}
	// Bad --k8s-secret fails BEFORE minting (no secret-id burned on a typo).
	if err := runCISeedApprole(seedApproleOpts{role: "r", k8sSecret: "not-a-ref"}); err == nil {
		t.Error("bad --k8s-secret must error")
	}
	// Missing root token: the mint cannot run.
	t.Setenv("OPENBAO_ROOT_TOKEN", "")
	if err := runCISeedApprole(seedApproleOpts{role: "r"}); err == nil {
		t.Error("missing OPENBAO_ROOT_TOKEN must error")
	}
}
