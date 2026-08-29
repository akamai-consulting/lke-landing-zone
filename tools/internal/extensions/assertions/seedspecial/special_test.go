package seedspecial

import (
	"github.com/spf13/cobra"

	"errors"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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

// The cobra surfaces and the extension declaration, which nothing exercised: the
// tests removed with the retired Kyverno scope-split had been carrying this
// package's coverage floor, and their absence exposed that the flag wiring and the
// declaration were never asserted at all.
//
// Cheap, but not nothing — the extension's grants are the capability fence, and a
// command whose flags do not bind is one an operator cannot steer.
func TestCommandSurfaces(t *testing.T) {
	for _, tc := range []struct {
		cmd   *cobra.Command
		use   string
		flags []string
	}{
		{ResolveHarborURLCmd(), "resolve-harbor-url", nil},
		{AuditPVCStorageClassCmd(), "audit-pvc-storageclass", nil},
	} {
		if tc.cmd.Use != tc.use {
			t.Errorf("Use = %q, want %q", tc.cmd.Use, tc.use)
		}
		if tc.cmd.RunE == nil {
			t.Errorf("%s has no RunE", tc.use)
		}
		for _, f := range tc.flags {
			if tc.cmd.Flags().Lookup(f) == nil {
				t.Errorf("%s is missing flag --%s", tc.use, f)
			}
		}
	}
}

// A GATE'S BINDING IS ITS FENCE. seed-special reads the cluster and writes only
// GitHub step output, so cluster-read is the whole grant — anything wider would be
// a capability nothing in the package uses.
func TestExtensionDeclaresOnlyClusterRead(t *testing.T) {
	e := Extension()
	if e.Name == "" {
		t.Fatal("extension has no name")
	}
	if len(e.Bindings) == 0 {
		t.Fatal("extension declares no bindings")
	}
	for _, b := range e.Bindings {
		for _, g := range b.Grants {
			if g != extension.ClusterRead {
				t.Errorf("binding %q declares %q — every call in this package is a `kubectl get`, "+
					"and the only writes are GitHub step outputs describing its own findings", b.Name, g)
			}
		}
	}
}

// TestResolveHarborURLManagedRemedyIsAchievable pins the REMEDY half of the
// divergence warning. The cross-check above already proves the warning FIRES on
// managed; what it never checked is whether the advice can be followed, and on
// managed neither half of the self-install text can be:
//
//   - "align vars.HARBOR_URL with it" — the managed host is
//     harbor.lke<id>.akamai-apl.net and the LKE id is new for every cluster, so a
//     static repo variable is stale the next time the cluster is rebuilt.
//   - "change the domainSuffix" — validateEnv rejects a managed env that sets one
//     ("domainSuffix must NOT be set"), so this is not merely wrong but invalid.
//
// A warning that asks for two impossible things teaches its reader to skip it,
// which is how a stale override survives: harbor.e2e.internal sat on the
// infra-e2e environment from 2026-06-09 until an e2e run surfaced the mismatch.
// The only achievable remedy is to unset the override and let discovery own it.
func TestResolveHarborURLManagedRemedyIsAchievable(t *testing.T) {
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
metadata: { name: t }
spec:
  instance: { upstreamOrg: akamai-consulting, repo: o/t, forge: github, templateVersion: v0.4.0 }
  defaults:
    cluster:
      k8sVersion: v1.33.6+lke7
      nodePool: { type: g8-dedicated-8-4, count: 3 }
`)
	// The real managed shape: managedAppPlatform with NO domainSuffix.
	write("environments/e2e.yaml", `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: e2e }
spec:
  cluster:
    clusterLabel: c-e2e
    region: us-sea
    bootstrap: { name: b-e2e, managedAppPlatform: true }
    objectStorage: { cluster: us-sea-1 }
`)
	t.Chdir(dir)
	// Discovery's single read: the otomi/otomi-api SSO_ISSUER, from which
	// ManagedDomainFromIssuer strips the bare domain.
	withExecOutput(t, func(_ string, _ ...string) ([]byte, error) {
		return []byte("https://keycloak.lke648821.akamai-apl.net/realms/otomi"), nil
	})
	t.Setenv("HARBOR_URL", "harbor.e2e.internal")

	var errOut string
	out := captureStdout(t, func() {
		errOut = captureStderr(t, func() {
			if err := RunResolveHarborURL("e2e"); err != nil {
				t.Fatalf("an override must not fail the preflight: %v", err)
			}
		})
	})
	_ = out

	if !strings.Contains(errOut, "::warning::") {
		t.Fatalf("a divergent override on managed must still warn:\n%s", errOut)
	}
	// It must name the real host, so the reader can see what discovery found.
	if !strings.Contains(errOut, "harbor.lke648821.akamai-apl.net") {
		t.Errorf("warning must name the discovered host:\n%s", errOut)
	}
	// The achievable instruction.
	if !strings.Contains(errOut, "UNSET vars.HARBOR_URL") {
		t.Errorf("managed remedy must tell the operator to unset the override:\n%s", errOut)
	}
	// The impossible one must be gone. Guarding the whole phrase (not the bare
	// word "domainSuffix", which legitimately appears in the self-install source
	// label) keeps this from passing for the wrong reason.
	if strings.Contains(errOut, "change the domainSuffix") {
		t.Errorf("managed remedy must not advise a domainSuffix validateEnv rejects:\n%s", errOut)
	}
}
