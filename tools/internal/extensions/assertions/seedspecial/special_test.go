package seedspecial

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfvars"
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
		if got := tfvars.Value(content, tc.key); got != tc.want {
			t.Errorf("tfvars.Value(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
	if got := tfvars.Value("", "obj_cluster"); got != "" {
		t.Errorf("empty content must yield empty, got %q", got)
	}
}

// ── seed-harbor-registry-s3 ───────────────────────────────────────────────────

func TestHarborRegistryS3Fields(t *testing.T) {
	got := credrotate.HarborRegistryS3Fields("acme", "primary", "us-ord-1", "AK", "SK")
	want := map[string]string{
		"access_key_id":     "AK",
		"secret_access_key": "SK",
		"bucket_name":       "acme-harbor-registry-primary",
		"endpoint":          "https://us-ord-1.linodeobjects.com",
		"region":            "us-ord-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("credrotate.HarborRegistryS3Fields = %v, want %v", got, want)
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

// ── mint-bootstrap-objkeys ────────────────────────────────────────────────────

func TestRunCIResolveHarborURL(t *testing.T) {
	dir := chdirTempDir(t)

	// vars.HARBOR_URL wins; nothing written to $GITHUB_ENV, no spec needed.
	t.Setenv("HARBOR_URL", "harbor.example.com")
	envFile := withGHAEnvFile(t)
	if err := RunResolveHarborURL("primary"); err != nil {
		t.Fatal(err)
	}
	if ghaEnvContains(t, envFile, "HARBOR_URL=") {
		t.Error("explicit HARBOR_URL must not be re-derived")
	}

	// Unset + no spec → hard error (the spec is mandatory; the tfvars
	// side-channel this used to fall back to was retired).
	t.Setenv("HARBOR_URL", "")
	if err := RunResolveHarborURL("primary"); err == nil {
		t.Error("missing spec must error")
	}

	// Unset → derived from the spec's domainSuffix and exported.
	writeResolveSpec(t, dir, "primary", "primary.internal")
	envFile = withGHAEnvFile(t)
	if err := RunResolveHarborURL("primary"); err != nil {
		t.Fatal(err)
	}
	if !ghaEnvContains(t, envFile, "HARBOR_URL=harbor.primary.internal") {
		t.Error("derived HARBOR_URL must be exported to $GITHUB_ENV")
	}

	// Env absent from the spec / empty domainSuffix → hard error.
	if err := RunResolveHarborURL("absent-region"); err == nil {
		t.Error("env absent from the spec must error")
	}
	if err := RunResolveHarborURL(""); err == nil {
		t.Error("missing --region must error")
	}
}

// writeResolveSpec writes a minimal split-layout spec (landingzone.yaml +
// environments/<env>.yaml) with just the domainSuffix resolve-harbor-url reads.
func writeResolveSpec(t *testing.T, dir, env, domainSuffix string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "landingzone.yaml"),
		[]byte("apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata:\n  name: itest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "environments"), 0o755); err != nil {
		t.Fatal(err)
	}
	cd := "apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: ClusterDefinition\nmetadata:\n  name: " + env +
		"\nspec:\n  cluster:\n    bootstrap:\n      domainSuffix: " + domainSuffix + "\n"
	if err := os.WriteFile(filepath.Join(dir, "environments", env+".yaml"), []byte(cd), 0o644); err != nil {
		t.Fatal(err)
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
	if err := RunAuditPVCStorageClass(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(sum)
	for _, want := range []string{
		"### PVCs not on the encrypted, Retain StorageClass",
		"NAMESPACE  PVC  STORAGECLASS",
		"data-harbor-redis-0",
		"gitea-shared",
		"**To remediate**",
		// THE SUMMARY MUST NAME THE CAUSE THAT EXISTS. It used to split findings by a
		// Kyverno policy's namespace scope and tell the reader an in-scope PVC meant
		// the webhook was late — a race that could not explain anything, because the
		// policy had not been applied since LLZ went managed-only. What remains is the
		// ordering story, which was always the real one.
		"**The cause is StorageClass ordering, not admission.**",
		"cluster.defaultStorageClass",
		// And it must not resurrect the webhook-timing explanation.
		"it was absent",
		// And it must say that recreate is the only remedy, since storageClassName is
		// immutable once bound.
		"is immutable once bound",
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
	if err := RunAuditPVCStorageClass(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(sum); len(b) != 0 {
		t.Errorf("clean audit must write no summary, got %q", b)
	}

	// kubectl failure → best-effort clean exit (the bash || true).
	withKubectl(t, func(string) ([]byte, error) { return nil, errors.New("no cluster") })
	if err := RunAuditPVCStorageClass(); err != nil {
		t.Errorf("kubectl failure must not fail the audit: %v", err)
	}
}

// TestResolveHarborURLWarnsOnDivergentOverride pins the cross-check. The
// in-cluster harbor-robot-provisioner gets HARBOR_HOST from
// clusterspec.RenderHarborHostPatch, which ALWAYS derives harbor.<domainSuffix>
// and ignores vars.HARBOR_URL. An override that diverges therefore points CI and
// the cluster at different registries, and kustomize.go's comment ("must keep
// this in step") was the only thing holding the two together.
func TestResolveHarborURLWarnsOnDivergentOverride(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("landingzone.yaml", `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { Name: t }
spec:
  instance: { upstreamOrg: akamai-consulting, repo: o/t, forge: github, templateVersion: v0.4.0 }
  defaults:
    cluster:
      k8sVersion: v1.33.6+lke7
      nodePool: { type: g8-dedicated-8-4, count: 3 }
`)
	write("environments/e2e.yaml", `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { Name: e2e }
spec:
  cluster:
    clusterLabel: c-e2e
    region: us-sea
    bootstrap: { Name: b-e2e, domainSuffix: e2e.example.com }
    objectStorage: { cluster: us-sea-1 }
`)
	t.Chdir(dir)

	t.Run("override matching the derivation is quiet", func(t *testing.T) {
		t.Setenv("HARBOR_URL", "harbor.e2e.example.com")
		errOut := captureStderr(t, func() {
			if err := RunResolveHarborURL("e2e"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		if strings.Contains(errOut, "::warning::") {
			t.Errorf("an override equal to harbor.<domainSuffix> must not warn:\n%s", errOut)
		}
	})

	t.Run("override diverging from the derivation warns", func(t *testing.T) {
		t.Setenv("HARBOR_URL", "registry.elsewhere.test")
		errOut := captureStderr(t, func() {
			if err := RunResolveHarborURL("e2e"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
		if !strings.Contains(errOut, "::warning::") {
			t.Errorf("a divergent override must warn — CI and the in-cluster provisioner would use different registries:\n%s", errOut)
		}
		for _, want := range []string{"registry.elsewhere.test", "harbor.e2e.example.com"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("warning should name both hosts, missing %q:\n%s", want, errOut)
			}
		}
	})
}

// THE KYVERNO SCOPE COUPLING TEST IS GONE, with the mechanism it pinned.
//
// It was a good coupling test: it read the ClusterPolicy's `namespaces:` list off
// disk and failed if the Go copy drifted, so the audit could never misattribute a
// PVC to a webhook that did not cover it. What it could not check was whether the
// policy was APPLIED anywhere — and it had not been since LLZ went managed-only.
// So it held two halves of a dead mechanism faithfully in sync, and the audit went
// on explaining PVCs with a webhook-timing story that could not be true.
//
// The lesson is not "fewer coupling tests". It is that a coupling test proves two
// things AGREE, never that either one RUNS — the same gap `delivered-consumer-guard`
// exists to close for delivered files. It read the policy off disk on every run and
// could not tell that no cluster ever received it.

func pvcNames(rows []pvcRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}
