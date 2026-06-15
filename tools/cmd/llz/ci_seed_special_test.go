package main

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTfvarsValue(t *testing.T) {
	content := `
# obj_cluster = "commented-out"
region       = "us-ord"
obj_cluster  = "us-ord-1" # trailing comment
obj_cluster  = "second-wins-not"
cluster_domain = "primary.internal"
unquoted = bare
`
	cases := []struct{ key, want string }{
		{"obj_cluster", "us-ord-1"}, // first assignment wins; comment line skipped
		{"cluster_domain", "primary.internal"},
		{"unquoted", "bare"},
		{"absent", ""},
	}
	for _, tc := range cases {
		if got := tfvarsValue(content, tc.key); got != tc.want {
			t.Errorf("tfvarsValue(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
	if got := tfvarsValue("", "obj_cluster"); got != "" {
		t.Errorf("empty content must yield empty, got %q", got)
	}
}

// ── seed-harbor-dockerconfig ──────────────────────────────────────────────────

func TestHarborDockerConfigJSON(t *testing.T) {
	// Byte-identical to the bash printf for scheme-stripped host + plain creds.
	got := harborDockerConfigJSON("https://harbor.primary.internal", "robot$ci", "s3cret")
	auth := base64.StdEncoding.EncodeToString([]byte("robot$ci:s3cret"))
	want := `{"auths":{"harbor.primary.internal":{"username":"robot$ci","password":"s3cret","auth":"` + auth + `"}}}`
	if got != want {
		t.Errorf("docker config:\n got %s\nwant %s", got, want)
	}
	// http:// strips too; no scheme passes through.
	if got := harborDockerConfigJSON("http://h.example", "u", "p"); !strings.Contains(got, `{"h.example":`) {
		t.Errorf("http scheme not stripped: %s", got)
	}
	if got := harborDockerConfigJSON("h.example", "u", "p"); !strings.Contains(got, `{"h.example":`) {
		t.Errorf("schemeless host mangled: %s", got)
	}
	// A quote in a credential must not produce invalid JSON (bash printf did).
	if got := harborDockerConfigJSON("h", `u"x`, "p"); !strings.Contains(got, `"username":"u\"x"`) {
		t.Errorf("quote not JSON-escaped: %s", got)
	}
}

func TestRunCISeedHarborDockerConfig(t *testing.T) {
	// Robot unseeded → summary skip, no kv put.
	puts := stubBaoSeedKV(t, "", "")
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")
	sum := withGHASummaryFile(t)
	if err := runCISeedHarborDockerConfig(); err != nil {
		t.Fatal(err)
	}
	if len(*puts) != 0 {
		t.Errorf("unseeded robot must skip, got %v", *puts)
	}
	b, _ := os.ReadFile(sum)
	if !strings.Contains(string(b), "secret/harbor/robot not yet populated — skipping buildah config seed.") {
		t.Errorf("summary missing skip note: %q", b)
	}

	// Robot present → one kv put with the rendered config_json.
	prev := baoExecFn
	var putArgs []string
	baoExecFn = func(_, _, _ string, args ...string) (string, string, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "-field=username"):
			return "robot$ci\n", "", nil
		case strings.Contains(joined, "-field=password"):
			return "s3cret\n", "", nil
		case strings.HasPrefix(joined, "kv put"):
			putArgs = args
			return "", "", nil
		}
		return "", "", errors.New("unexpected: " + joined)
	}
	t.Cleanup(func() { baoExecFn = prev })
	t.Setenv("HARBOR_URL", "https://harbor.primary.internal")
	if err := runCISeedHarborDockerConfig(); err != nil {
		t.Fatal(err)
	}
	want := "config_json=" + harborDockerConfigJSON("https://harbor.primary.internal", "robot$ci", "s3cret")
	if len(putArgs) != 4 || putArgs[2] != "secret/harbor/docker-config" || putArgs[3] != want {
		t.Errorf("kv put = %v, want path secret/harbor/docker-config field %q", putArgs, want)
	}
}

// ── seed-harbor-registry-s3 ───────────────────────────────────────────────────

func TestHarborRegistryS3Fields(t *testing.T) {
	got := harborRegistryS3Fields("primary", "us-ord-1", "AK", "SK")
	want := map[string]string{
		"access_key_id":     "AK",
		"secret_access_key": "SK",
		"bucket_name":       "platform-harbor-registry-primary",
		"endpoint":          "https://us-ord-1.linodeobjects.com",
		"region":            "us-ord-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("harborRegistryS3Fields = %v, want %v", got, want)
	}
}

// chdirTempDir moves the test into a fresh temp dir (the commands resolve tfvars
// relative to the workflow's checkout root).
func chdirTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return dir
}

func writeTFVars(t *testing.T, dir, sub, region, content string) {
	t.Helper()
	p := filepath.Join(dir, "terraform-iac-bootstrap", sub)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, region+".tfvars"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunCISeedHarborRegistryS3(t *testing.T) {
	dir := chdirTempDir(t)
	t.Setenv("OPENBAO_ROOT_TOKEN", "root")

	// Missing env secrets → summary + deferred failure, exit 0, no kv put.
	puts := stubBaoSeedKV(t, "", "")
	t.Setenv("HARBOR_REGISTRY_S3_ACCESS_KEY", "")
	t.Setenv("HARBOR_REGISTRY_S3_SECRET_KEY", "")
	envFile := withGHAEnvFile(t)
	sum := withGHASummaryFile(t)
	if err := runCISeedHarborRegistryS3("primary"); err != nil {
		t.Fatalf("missing secrets must defer, not fail: %v", err)
	}
	if !ghaEnvContains(t, envFile, "BOOTSTRAP_ERRORS=true") {
		t.Error("missing secrets must set BOOTSTRAP_ERRORS=true")
	}
	b, _ := os.ReadFile(sum)
	if !strings.Contains(string(b), "skipping secret/harbor/registry-s3") ||
		!strings.Contains(string(b), "Add them as infra-primary environment secrets and re-run.") {
		t.Errorf("summary missing remediation: %q", b)
	}
	if len(*puts) != 0 {
		t.Errorf("missing secrets must not kv put, got %v", *puts)
	}

	// Secrets present but obj_cluster unresolvable → hard error (exit 1).
	t.Setenv("HARBOR_REGISTRY_S3_ACCESS_KEY", "AK")
	t.Setenv("HARBOR_REGISTRY_S3_SECRET_KEY", "SK")
	if err := runCISeedHarborRegistryS3("primary"); err == nil {
		t.Error("missing obj_cluster must hard-fail")
	}

	// Full path: tfvars resolve, all 5 fields land on one kv put argv.
	writeTFVars(t, dir, "object-storage", "primary", `obj_cluster = "us-ord-1"`)
	puts = stubBaoSeedKV(t, "", "")
	if err := runCISeedHarborRegistryS3("primary"); err != nil {
		t.Fatal(err)
	}
	if len(*puts) != 1 {
		t.Fatalf("want one kv put, got %d", len(*puts))
	}
	got := strings.Join((*puts)[0], " ")
	want := "kv put secret/harbor/registry-s3 access_key_id=AK bucket_name=platform-harbor-registry-primary " +
		"endpoint=https://us-ord-1.linodeobjects.com region=us-ord-1 secret_access_key=SK"
	if got != want {
		t.Errorf("kv put argv:\n got %q\nwant %q", got, want)
	}

	if err := runCISeedHarborRegistryS3(""); err == nil {
		t.Error("missing --region must error")
	}
}

// ── resolve-harbor-url ────────────────────────────────────────────────────────

func TestRunCIResolveHarborURL(t *testing.T) {
	dir := chdirTempDir(t)

	// vars.HARBOR_URL wins; nothing written to $GITHUB_ENV.
	t.Setenv("HARBOR_URL", "harbor.example.com")
	envFile := withGHAEnvFile(t)
	if err := runCIResolveHarborURL("primary"); err != nil {
		t.Fatal(err)
	}
	if ghaEnvContains(t, envFile, "HARBOR_URL=") {
		t.Error("explicit HARBOR_URL must not be re-derived")
	}

	// Unset → derived from cluster_domain and exported.
	t.Setenv("HARBOR_URL", "")
	writeTFVars(t, dir, "cluster-bootstrap", "primary", `cluster_domain = "primary.internal"`)
	envFile = withGHAEnvFile(t)
	if err := runCIResolveHarborURL("primary"); err != nil {
		t.Fatal(err)
	}
	if !ghaEnvContains(t, envFile, "HARBOR_URL=harbor.primary.internal") {
		t.Error("derived HARBOR_URL must be exported to $GITHUB_ENV")
	}

	// Neither available → hard error.
	if err := runCIResolveHarborURL("absent-region"); err == nil {
		t.Error("missing cluster_domain must error")
	}
	if err := runCIResolveHarborURL(""); err == nil {
		t.Error("missing --region must error")
	}
}

// ── audit-pvc-storageclass ────────────────────────────────────────────────────

const pvcFixture = `{
  "items": [
    {"metadata":{"namespace":"harbor","name":"data-harbor-redis-0"},
     "spec":{"storageClassName":"linode-block-storage"}},
    {"metadata":{"namespace":"llz-openbao","name":"data-platform-openbao-0"},
     "spec":{"storageClassName":"block-storage-retain"}},
    {"metadata":{"namespace":"gitea","name":"gitea-shared"},
     "spec":{}}
  ]
}`

func TestParsePVCListAndFilter(t *testing.T) {
	rows, err := parsePVCList([]byte(pvcFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := []pvcRow{
		{"harbor", "data-harbor-redis-0", "linode-block-storage"},
		{"llz-openbao", "data-platform-openbao-0", "block-storage-retain"},
		{"gitea", "gitea-shared", "<none>"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("parsePVCList = %v, want %v", rows, want)
	}
	escaped := escapedPVCs(rows, "block-storage-retain")
	if len(escaped) != 2 || escaped[0].Namespace != "harbor" || escaped[1].StorageClass != "<none>" {
		t.Errorf("escapedPVCs = %v", escaped)
	}
	if _, err := parsePVCList([]byte("not json")); err == nil {
		t.Error("bad JSON must error")
	}
}

func TestRenderPVCTable(t *testing.T) {
	lines := renderPVCTable([]pvcRow{
		{"harbor", "data-harbor-redis-0", "linode-block-storage"},
		{"gitea", "gitea-shared", "<none>"},
	})
	want := []string{
		"harbor  data-harbor-redis-0  linode-block-storage",
		"gitea   gitea-shared         <none>",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("renderPVCTable:\n got %q\nwant %q", lines, want)
	}
}

func TestRunCIAuditPVCStorageClass(t *testing.T) {
	// Escaped PVCs → summary block with table + remediation.
	withKubectl(t, func(a string) ([]byte, error) {
		if a == "get pvc -A -o json" {
			return []byte(pvcFixture), nil
		}
		return nil, errors.New("unexpected: " + a)
	})
	sum := withGHASummaryFile(t)
	if err := runCIAuditPVCStorageClass(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(sum)
	for _, want := range []string{
		"### PVCs that escaped the Kyverno encryption mutation",
		"NAMESPACE  PVC  STORAGECLASS",
		"data-harbor-redis-0",
		"gitea-shared",
		"**To remediate** (per-workload, irreversible for that data):",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("summary missing %q:\n%s", want, b)
		}
	}
	if strings.Contains(string(b), "data-platform-openbao-0") {
		t.Error("compliant PVCs must not be reported")
	}

	// All compliant → no summary written.
	withKubectl(t, func(string) ([]byte, error) {
		return []byte(`{"items":[{"metadata":{"namespace":"a","name":"b"},"spec":{"storageClassName":"block-storage-retain"}}]}`), nil
	})
	sum = withGHASummaryFile(t)
	if err := runCIAuditPVCStorageClass(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(sum); len(b) != 0 {
		t.Errorf("clean audit must write no summary, got %q", b)
	}

	// kubectl failure → best-effort clean exit (the bash || true).
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("no cluster") })
	if err := runCIAuditPVCStorageClass(); err != nil {
		t.Errorf("kubectl failure must not fail the audit: %v", err)
	}
}
