package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixWrite writes a fixture file under root at the slash-relative path rel.
func fixWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const esFixtureExternalSecret = `apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: grafana-admin
spec:
  data:
    - secretKey: admin-user
      remoteRef:
        key: grafana/admin
        property: user
    - secretKey: admin-password
      remoteRef:
        key: grafana/admin
        property: password
    - secretKey: whole-secret
      remoteRef:
        key: otel/ingress
`

func TestCollectExternalSecretRefs(t *testing.T) {
	root := t.TempDir()
	fixWrite(t, root, "apl-values/env/secrets.yaml", esFixtureExternalSecret)
	// Rendered chart output is scanned too (only *.yaml, never /charts/ subtrees).
	fixWrite(t, root, "rendered/llz/templates/es.yaml",
		"kind: ExternalSecret\n  remoteRef:\n    key: harbor/admin\n    property: password\n")
	fixWrite(t, root, "rendered/llz/charts/dep/es.yaml",
		"kind: ExternalSecret\n  remoteRef:\n    key: vendored/skipme\n")
	fixWrite(t, root, "apl-values/env/not-an-es.yaml", "kind: ConfigMap\n  remoteRef:\n    key: nope\n")
	fixWrite(t, root, "apl-values/env/wrong-ext.yml", esFixtureExternalSecret)

	refs := collectExternalSecretRefs(root, "rendered")
	want := map[esRef][]string{}
	want[esRef{key: "grafana/admin", prop: "user", hasProp: true}] = []string{"apl-values/env/secrets.yaml"}
	want[esRef{key: "grafana/admin", prop: "password", hasProp: true}] = []string{"apl-values/env/secrets.yaml"}
	want[esRef{key: "otel/ingress"}] = []string{"apl-values/env/secrets.yaml"}
	want[esRef{key: "harbor/admin", prop: "password", hasProp: true}] = []string{"rendered/llz/templates/es.yaml"}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("refs = %#v\nwant %#v", refs, want)
	}
}

func TestCollectSeeded(t *testing.T) {
	root := t.TempDir()
	fixWrite(t, root, "bootstrap.yml", strings.Join([]string{
		"      - run: |",
		`          llz openbao exec -- kv put secret/grafana/admin user="admin" password="$PASS"`,
		`          bao kv put secret/otel/ingress \`,
		`            token="$OTEL_TOKEN" \`,
		`            tls_crt="$CRT"`,
		`          echo done`,
	}, "\n"))
	paths, fields, err := collectSeeded([]string{filepath.Join(root, "bootstrap.yml")})
	if err != nil {
		t.Fatal(err)
	}
	if !paths["grafana/admin"] || !paths["otel/ingress"] || len(paths) != 2 {
		t.Errorf("paths = %v", paths)
	}
	if !fields["grafana/admin"]["user"] || !fields["grafana/admin"]["password"] {
		t.Errorf("grafana fields = %v", fields["grafana/admin"])
	}
	// Backslash continuations are joined, so multi-line fields are seen.
	if !fields["otel/ingress"]["token"] || !fields["otel/ingress"]["tls_crt"] {
		t.Errorf("otel fields = %v", fields["otel/ingress"])
	}

	if _, _, err := collectSeeded([]string{filepath.Join(root, "absent.yml")}); err == nil {
		t.Error("a missing seeding source must be an error")
	}
}

// The `llz ci bao-seed --path secret/<path> --field <name>=…` step (which
// replaced most inline `bao kv put` blocks) must be recognized as seeding its
// path and fields, including across backslash continuations.
func TestCollectSeededBaoSeed(t *testing.T) {
	root := t.TempDir()
	fixWrite(t, root, "bootstrap.yml", strings.Join([]string{
		"      - run: |",
		`          llz ci bao-seed --path secret/cert-automation/github-token \`,
		`            --field token=env:OPENBAO_SECRETS_WRITE_TOKEN`,
		"      - run: |",
		`          llz ci bao-seed --path secret/approle/rotation-secrets \`,
		`            --on-missing skip \`,
		`            --field github_token=env:OPENBAO_SECRETS_WRITE_TOKEN`,
	}, "\n"))
	paths, fields, err := collectSeeded([]string{filepath.Join(root, "bootstrap.yml")})
	if err != nil {
		t.Fatal(err)
	}
	if !paths["cert-automation/github-token"] || !paths["approle/rotation-secrets"] {
		t.Errorf("paths = %v", paths)
	}
	if !fields["cert-automation/github-token"]["token"] {
		t.Errorf("cert-automation fields = %v", fields["cert-automation/github-token"])
	}
	if !fields["approle/rotation-secrets"]["github_token"] {
		t.Errorf("approle fields = %v", fields["approle/rotation-secrets"])
	}
}

func TestCollectSeededGo(t *testing.T) {
	root := t.TempDir()
	fixWrite(t, root, "ci_harbor.go", `package main
func seed() {
	baoKVPutFn("secret/harbor/docker-config", map[string]string{
		"config_json": cfg,
	})
	specs := []harborRobotSpec{
		{kvPath: "secret/harbor/robot"},
		{kvPath: "secret/harbor/pull-robot"},
	}
	for _, spec := range specs {
		_ = baoKVPutFn(spec.kvPath, map[string]string{
			"name":  spec.name,
			"token": tok,
		})
	}
}
`)
	paths, fields, err := collectSeededGo(filepath.Join(root, "ci_harbor.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"harbor/docker-config", "harbor/robot", "harbor/pull-robot"} {
		if !paths[p] {
			t.Errorf("path %s not collected (have %v)", p, paths)
		}
	}
	if !fields["harbor/docker-config"]["config_json"] {
		t.Errorf("direct-put fields = %v", fields["harbor/docker-config"])
	}
	// Spec-seeded paths share the field set of the baoKVPutFn(spec.kvPath, …) site.
	for _, p := range []string{"harbor/robot", "harbor/pull-robot"} {
		if !fields[p]["name"] || !fields[p]["token"] {
			t.Errorf("%s fields = %v", p, fields[p])
		}
	}
}

func TestCollectPolicyPaths(t *testing.T) {
	root := t.TempDir()
	fixWrite(t, root, "configure.go", "const policy = `"+strings.Join([]string{
		`path "secret/data/grafana/admin"     { capabilities = ["read"] }`,
		`path "secret/metadata/grafana/admin" { capabilities = ["read", "list"] }`,
		`path "secret/data/otel/ingress" {`,
		`  capabilities = ["read", "create"]`,
		`}`,
		`path "unrelated/data/x" { capabilities = ["read"] }`,
	}, "\n")+"`\n")
	got, err := collectPolicyPaths(filepath.Join(root, "configure.go"))
	if err != nil {
		t.Fatal(err)
	}
	want := esPolicy{
		"grafana/admin": {
			"data":     {"read": true},
			"metadata": {"read": true, "list": true},
		},
		"otel/ingress": {
			"data": {"read": true, "create": true},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("policies = %#v\nwant %#v", got, want)
	}
}

func TestCollectModuleCIReadPaths(t *testing.T) {
	root := t.TempDir()
	fixWrite(t, root, "variables.tf", strings.Join([]string{
		`variable "ci_read_paths" {`,
		`  description = "paths"`,
		`  type        = list(string)`,
		`  default = [`,
		`    "grafana/admin",`,
		`    "otel/ingress",`,
		`  ]`,
		`}`,
	}, "\n"))
	got, err := collectModuleCIReadPaths(filepath.Join(root, "variables.tf"))
	if err != nil {
		t.Fatal(err)
	}
	want := esPolicy{
		"grafana/admin": {"data": {"read": true}, "metadata": {"read": true, "list": true}},
		"otel/ingress":  {"data": {"read": true}, "metadata": {"read": true, "list": true}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("module coverage = %#v\nwant %#v", got, want)
	}

	// No ci_read_paths variable → empty coverage, no error.
	fixWrite(t, root, "empty.tf", `variable "other" { default = [] }`)
	if got, err := collectModuleCIReadPaths(filepath.Join(root, "empty.tf")); err != nil || len(got) != 0 {
		t.Errorf("empty module = %v, %v", got, err)
	}
}

func TestValidatePolicyCoverage(t *testing.T) {
	policy := map[string]esPolicy{
		"a-policy": {
			"covered": {"data": {"read": true}, "metadata": {"read": true, "list": true}},
			"partial": {"data": {"read": true}, "metadata": {"read": true}},
		},
		"b-policy": {
			"covered": {"data": {"read": true}, "metadata": {"read": true, "list": true}},
		},
	}

	var buf bytes.Buffer
	if n := validatePolicyCoverage("covered", policy, []string{"f.yaml"}, &buf); n != 0 || buf.Len() != 0 {
		t.Errorf("covered: n=%d out=%q", n, buf.String())
	}

	buf.Reset()
	// "partial": metadata list missing in a-policy; absent entirely from b-policy.
	if n := validatePolicyCoverage("partial", policy, []string{"f.yaml"}, &buf); n != 3 {
		t.Errorf("partial: n=%d", n)
	}
	out := buf.String()
	for _, want := range []string{
		"::error file=f.yaml::KV path 'partial' is not covered by a-policy: expected path 'secret/metadata/partial' with read and list capabilities\n",
		"::error file=f.yaml::KV path 'partial' is not covered by b-policy: expected path 'secret/data/partial' with read capability\n",
		"::error file=f.yaml::KV path 'partial' is not covered by b-policy: expected path 'secret/metadata/partial' with read and list capabilities\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing annotation %q in:\n%s", want, out)
		}
	}
	// Policies are reported in sorted label order.
	if strings.Index(out, "a-policy") > strings.Index(out, "b-policy") {
		t.Errorf("policy labels not sorted:\n%s", out)
	}
}

// esFixtureRepo builds a minimal repo where every ref is seeded + covered.
func esFixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	fixWrite(t, root, "apl-values/env/secrets.yaml", esFixtureExternalSecret)
	fixWrite(t, root, ".github/workflows/llz-bootstrap-openbao.yml", strings.Join([]string{
		`          llz openbao exec -- kv put secret/grafana/admin user="$U" password="$P"`,
		`          llz openbao exec -- kv put secret/otel/ingress token="$T"`,
		`          llz openbao exec -- kv put secret/infra/github-dispatch-token token="$D"`,
	}, "\n"))
	fixWrite(t, root, ".github/workflows/llz-bootstrap-dns.yml",
		`          llz openbao exec -- kv put secret/certmanager/dns01 token="$LINODE_DNS_TOKEN"`+"\n")
	fixWrite(t, root, "tools/cmd/llz/ci_harbor.go",
		"package main\nvar _ = baoKVPutFn(\"secret/harbor/admin\", map[string]string{\"password\": p})\n")

	var policy strings.Builder
	policy.WriteString("package main\nconst policyPlatformCI = `\n")
	var ciReadPaths strings.Builder
	ciReadPaths.WriteString("variable \"ci_read_paths\" {\n  default = [\n")
	for _, p := range []string{"grafana/admin", "otel/ingress", "infra/github-dispatch-token", "certmanager/dns01", "harbor/admin"} {
		policy.WriteString(`path "secret/data/` + p + `" { capabilities = ["read"] }` + "\n")
		policy.WriteString(`path "secret/metadata/` + p + `" { capabilities = ["read", "list"] }` + "\n")
		ciReadPaths.WriteString(`    "` + p + `",` + "\n")
	}
	policy.WriteString("`\n")
	ciReadPaths.WriteString("  ]\n}\n")
	fixWrite(t, root, "tools/cmd/llz/ci_openbao_configure.go", policy.String())
	fixWrite(t, root, "terraform-modules/llz-openbao/variables.tf", ciReadPaths.String())
	return root
}

func TestRunCIExternalSecretPathsHappyPath(t *testing.T) {
	t.Setenv("RENDER_DIR", "")
	root := esFixtureRepo(t)
	var buf bytes.Buffer
	if err := runCIExternalSecretPaths(root, &buf); err != nil {
		t.Fatalf("happy path: %v\n%s", err, buf.String())
	}
	want := strings.Join([]string{
		"  ok: grafana/admin.password",
		"  ok: grafana/admin.user",
		"  ok: otel/ingress",
		"  ok (seeded policy): certmanager/dns01",
		"  ok (seeded policy): harbor/admin",
		"  ok (seeded policy): infra/github-dispatch-token",
		"",
		"All ExternalSecret refs and bootstrap-seeded paths are policy-covered.",
		"",
	}, "\n")
	if buf.String() != want {
		t.Errorf("output:\n%q\nwant:\n%q", buf.String(), want)
	}
}

func TestRunCIExternalSecretPathsFailures(t *testing.T) {
	t.Setenv("RENDER_DIR", "")
	root := esFixtureRepo(t)
	// An unseeded ref, a missing property, and an uncovered seeded path.
	fixWrite(t, root, "apl-values/env/more.yaml", strings.Join([]string{
		"kind: ExternalSecret",
		"  remoteRef:",
		"    key: never/seeded",
		"  remoteRef:",
		"    key: grafana/admin",
		"    property: missing_field",
	}, "\n"))
	fixWrite(t, root, ".github/workflows/llz-bootstrap-dns.yml",
		`          bao kv put secret/uncovered/path field="x"`+"\n")

	var buf bytes.Buffer
	err := runCIExternalSecretPaths(root, &buf)
	if err == nil {
		t.Fatalf("must fail:\n%s", buf.String())
	}
	// 1 unseeded key + 1 missing property + 4 uncovered-policy grants for the
	// seeded-but-unreferenced uncovered/path (2 grants × 2 policy sources).
	if err.Error() != "6 ExternalSecret ref(s) failed seed or policy validation" {
		t.Errorf("err = %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"::error file=apl-values/env/more.yaml::ExternalSecret remoteRef.key 'grafana/admin' property 'missing_field' is not written by any 'bao kv put secret/grafana/admin' step in bootstrap-openbao.yml or bootstrap-dns.yml\n",
		"::error file=apl-values/env/more.yaml::ExternalSecret remoteRef.key 'never/seeded' is not seeded by any bootstrap workflow — add a 'bao kv put secret/never/seeded' step to bootstrap-openbao.yml or bootstrap-dns.yml, or add to MANUAL_PATHS if intentionally manual\n",
		"::error file=tools/cmd/llz/ci_openbao_configure.go::KV path 'uncovered/path' is not covered by llz ci bao-configure (ci_openbao_configure.go): expected path 'secret/data/uncovered/path' with read capability\n",
		"::error file=tools/cmd/llz/ci_openbao_configure.go::KV path 'uncovered/path' is not covered by terraform-modules/llz-openbao: expected path 'secret/metadata/uncovered/path' with read and list capabilities\n",
		"\n6 ExternalSecret ref(s) failed seed or policy validation.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// certmanager/dns01 was replaced in the dns workflow, so it is no longer
	// seeded — but it is also no longer referenced, so it simply drops out.
	if strings.Contains(out, "ok (seeded policy): certmanager/dns01") {
		t.Errorf("dns01 should no longer be reported seeded:\n%s", out)
	}
}

func TestRunCIExternalSecretPathsInstanceTemplateLayout(t *testing.T) {
	t.Setenv("RENDER_DIR", "")
	// The same fixture works when instance content lives under instance-template/.
	flat := esFixtureRepo(t)
	root := t.TempDir()
	for _, rel := range []string{
		".github/workflows/llz-bootstrap-openbao.yml",
		".github/workflows/llz-bootstrap-dns.yml",
	} {
		b, err := os.ReadFile(filepath.Join(flat, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		fixWrite(t, root, "instance-template/"+rel, string(b))
	}
	for _, rel := range []string{
		"tools/cmd/llz/ci_harbor.go",
		"tools/cmd/llz/ci_openbao_configure.go",
		"terraform-modules/llz-openbao/variables.tf",
		"apl-values/env/secrets.yaml",
	} {
		b, err := os.ReadFile(filepath.Join(flat, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		fixWrite(t, root, rel, string(b))
	}
	var buf bytes.Buffer
	if err := runCIExternalSecretPaths(root, &buf); err != nil {
		t.Fatalf("instance-template layout: %v\n%s", err, buf.String())
	}
}

// TestExternalSecretPathsRealRepo runs the validator against this repository —
// the same gate `make externalsecret-paths-check` enforces (minus the rendered
// charts, which need helm; the seeded-path policy coverage is fully exercised).
func TestExternalSecretPathsRealRepo(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".github", "workflows", "llz-bootstrap-openbao.yml")); err != nil {
		t.Skip("template repo layout not present")
	}
	t.Setenv("RENDER_DIR", "")
	var buf bytes.Buffer
	if err := runCIExternalSecretPaths(root, &buf); err != nil {
		t.Errorf("template repo validation failed: %v\n%s", err, buf.String())
	}
	t.Log(buf.String())
}
