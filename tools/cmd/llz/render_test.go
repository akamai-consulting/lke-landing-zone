package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// dealign collapses `tofu fmt`'s `=` column padding (key    = val → key = val) so
// the rendered-tfvars assertions match the canonical single-space form whether or
// not fmt aligned the output (it only runs when tofu/terraform is on PATH).
var alignRE = regexp.MustCompile(` +=`)

func dealign(s string) string { return alignRE.ReplaceAllString(s, " =") }

func TestApplyAssigns(t *testing.T) {
	base := "region = \"PLACEHOLDER\"\n# obj_key_rotation_days = 120\n"
	out := applyAssigns(base, []clusterspec.Assign{
		{Key: "region", Val: `"us-ord"`},          // replaces existing line
		{Key: "obj_key_rotation_days", Val: "90"}, // commented in example → appended
	})
	if !strings.Contains(out, `region = "us-ord"`) {
		t.Errorf("region not replaced:\n%s", out)
	}
	if strings.Contains(out, `"PLACEHOLDER"`) {
		t.Errorf("placeholder survived:\n%s", out)
	}
	if !strings.Contains(out, "obj_key_rotation_days = 90") {
		t.Errorf("commented key not appended:\n%s", out)
	}
}

const renderSpec = `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
  environments:
    prod:
      cluster:
        clusterLabel: platform-prod
        region: us-ord
        k8sVersion: v1.33.6+lke7
        nodePool: { type: g8-dedicated-8-4, count: 5 }
        bootstrap: { name: platform-prod, domainSuffix: prod.example.com }
        objectStorage: { cluster: us-ord-7 }
`

// renderAndWrite drives the render exactly as `llz render` does — renderTargets
// (the one definition of what a render produces) piped into writeTargets.
func renderAndWrite(t *testing.T, lz *clusterspec.LandingZone, envs []string, tfDir, aplDir string, tfvarsOnly bool) {
	t.Helper()
	targets, err := renderTargets(lz, envs, tfDir, aplDir, tfvarsOnly)
	if err != nil {
		t.Fatalf("renderTargets: %v", err)
	}
	if err := writeTargets(targets, tfDir, "", false); err != nil {
		t.Fatalf("writeTargets: %v", err)
	}
}

func TestRenderEnvTfvars(t *testing.T) {
	// The terraform.tfvars.example base now comes from the embedded tfroots package
	// (it no longer ships in the instance); the per-env <env>.tfvars land in tfDir.
	root := t.TempDir()
	tfDir := filepath.Join(root, "terraform-iac-bootstrap")

	lz, err := clusterspec.Decode([]byte(renderSpec))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	renderAndWrite(t, lz, []string{"prod"}, tfDir, filepath.Join(root, "apl-values"), true)

	read := func(root string) string {
		b, err := os.ReadFile(filepath.Join(tfDir, root, "prod.tfvars"))
		if err != nil {
			t.Fatalf("read %s/prod.tfvars: %v", root, err)
		}
		return dealign(string(b)) // tolerate tofu fmt's `=` alignment
	}
	cluster := read("cluster")
	for _, want := range []string{`cluster_label = "platform-prod"`, `region = "us-ord"`, `node_count = 5`} {
		if !strings.Contains(cluster, want) {
			t.Errorf("cluster/prod.tfvars missing %q:\n%s", want, cluster)
		}
	}
	// The cluster-bootstrap tfvars root was retired with its Terraform workspace
	// (the in-cluster bootstrap now runs via `llz ci bootstrap-cluster`, spec-driven).
	obj := read("object-storage")
	for _, want := range []string{`region_suffix = "prod"`, `obj_cluster = "us-ord-7"`} {
		if !strings.Contains(obj, want) {
			t.Errorf("object-storage/prod.tfvars missing %q:\n%s", want, obj)
		}
	}
}

func TestRenderManifest_AndDriftCheck(t *testing.T) {
	root := t.TempDir()
	aplDir := filepath.Join(root, "apl-values")
	lz, err := clusterspec.Decode([]byte(`
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
  environments:
    prod:
      cluster:
        clusterLabel: prod
        region: us-ord
        k8sVersion: v1.33.6+lke7
        nodePool: { type: t, count: 3 }
        bootstrap: { name: platform-prod, managedAppPlatform: true, managedApps: [loki] }
        objectStorage: { cluster: us-ord-1 }
      components:
        harbor: { enabled: false }
`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := os.MkdirAll(aplDir, 0o755); err != nil {
		t.Fatal(err)
	}

	renderAndWrite(t, lz, []string{"prod"}, filepath.Join(root, "terraform-iac-bootstrap"), aplDir, false)

	kust, err := os.ReadFile(filepath.Join(aplDir, "prod", "manifest", "kustomization.yaml"))
	if err != nil {
		t.Fatalf("read kustomization: %v", err)
	}
	// Thin overlay over the shared base; harbor disabled → no harbor App CR, but the
	// other carved Apps (observability, reconciler, externalsecrets) are referenced.
	if !strings.Contains(string(kust), "../../../../platform-apl/manifest") || strings.Contains(string(kust), "llz-harbor.yaml") {
		t.Errorf("kustomization wrong (thin overlay, harbor dropped):\n%s", kust)
	}
	if !strings.Contains(string(kust), "llz-observability.yaml") {
		t.Errorf("kustomization missing enabled carved App CR:\n%s", kust)
	}
	// The carved observability App CR + its self-contained source root were written.
	obsApp, err := os.ReadFile(filepath.Join(aplDir, "prod", "manifest", "llz-observability.yaml"))
	if err != nil || !strings.Contains(string(obsApp), "path: apl-values/prod/apps/observability") {
		t.Errorf("carved observability App CR missing/wrong (err=%v):\n%s", err, obsApp)
	}
	obsKust, err := os.ReadFile(filepath.Join(aplDir, "prod", "apps", "observability", "kustomization.yaml"))
	if err != nil || !strings.Contains(string(obsKust), "../../../../../platform-apl/components/observability") {
		t.Errorf("carved observability source root missing/wrong (err=%v):\n%s", err, obsKust)
	}

	// apl-core values.yaml is NOT rendered on the managed platform (apl-core owns its
	// own values; the platform-bootstrap App syncs only the manifest/ tree).
	if _, err := os.Stat(filepath.Join(aplDir, "prod", "values.yaml")); err == nil {
		t.Error("managed render must NOT emit an apl-core values.yaml (Linode owns apl-core values)")
	}

	// Freshly rendered → no drift.
	if err := checkManifestDrift(lz, aplDir, []string{"prod"}); err != nil {
		t.Fatalf("expected no drift after render; got %v", err)
	}
	// Tamper → drift detected.
	if err := os.WriteFile(filepath.Join(aplDir, "prod", "manifest", "kustomization.yaml"), []byte("hand-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkManifestDrift(lz, aplDir, []string{"prod"}); err == nil {
		t.Error("expected drift error after tampering with the committed kustomization")
	}
}

// The write path, `--check` and `--diff` all consume renderTargets, so `--check`
// covers EVERY target — including the tfvars it used to skip while `--diff` diffed
// them (the two paths disagreed about what a render produces).
func TestRenderTargetsDriveCheck_IncludingTfvars(t *testing.T) {
	root := t.TempDir()
	tfDir := filepath.Join(root, "terraform-iac-bootstrap")
	aplDir := filepath.Join(root, "apl-values")

	lz, err := clusterspec.Decode([]byte(renderSpec))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	renderAndWrite(t, lz, []string{"prod"}, tfDir, aplDir, false)

	// The same predicate `llz render --check` uses: absence is drift only outside the
	// gitignored terraform-iac-bootstrap build artifacts.
	check := func() error {
		targets, err := renderTargets(lz, []string{"prod"}, tfDir, aplDir, false)
		if err != nil {
			t.Fatalf("renderTargets: %v", err)
		}
		return reportDrift(targets, func(p string) bool {
			return !strings.HasPrefix(p, tfDir+string(filepath.Separator))
		})
	}
	// Freshly rendered → the check path sees no drift in ANY target.
	if err := check(); err != nil {
		t.Fatalf("expected no drift right after a render; got %v", err)
	}
	// Hand-edit a rendered tfvars → the check path now catches it (it used to look at
	// the committed apl-values only).
	tfvars := filepath.Join(tfDir, "cluster", "prod.tfvars")
	if err := os.WriteFile(tfvars, []byte("hand-edited = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Error("expected --check to report drift on a hand-edited <env>.tfvars")
	}
	// But a MISSING build artifact is not drift — on a fresh checkout none of the
	// gitignored tfvars/TF roots exist yet, and demanding them would fail every
	// instance's check.
	if err := os.RemoveAll(tfDir); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Errorf("un-rendered (gitignored) build artifacts must not count as drift; got %v", err)
	}
	// A missing COMMITTED apl-values file still is drift.
	if err := os.Remove(filepath.Join(aplDir, "prod", "manifest", "kustomization.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Error("expected drift for a missing committed apl-values target")
	}
}

func TestHasHCLKey(t *testing.T) {
	content := "region = \"us-ord\"\nnode_count=3\n# commented = 1\n  indented = 2\nnode_countx = 9\n"
	for _, k := range []string{"region", "node_count"} {
		if !hasHCLKey(content, k) {
			t.Errorf("hasHCLKey(%q) = false, want true", k)
		}
	}
	// A commented-out key, an indented one, and a mere prefix of a longer key are all
	// misses — the assignment must start the line (applyAssigns appends instead).
	for _, k := range []string{"commented", "indented", "node_coun"} {
		if hasHCLKey(content, k) {
			t.Errorf("hasHCLKey(%q) = true, want false", k)
		}
	}
}

// lineDiff trims the common prefix before its m×k LCS matrix; the emitted hunks must
// be byte-identical to what the untrimmed DP produced.
func TestLineDiff_CommonPrefixTrimmed(t *testing.T) {
	var oldB, newB strings.Builder
	for i := range 200 {
		fmt.Fprintf(&oldB, "line %d\n", i)
		fmt.Fprintf(&newB, "line %d\n", i)
	}
	oldB.WriteString("tail-old\n")
	newB.WriteString("tail-new\n")

	d := lineDiff(oldB.String(), newB.String())
	// The change and its context survive; the long identical run collapses to "…".
	for _, want := range []string{"- tail-old", "+ tail-new", "line 199", "…"} {
		if !strings.Contains(d, want) {
			t.Errorf("lineDiff missing %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "line 100") {
		t.Errorf("unchanged run beyond the context window should be collapsed:\n%s", d)
	}
	// The greedy backtrack breaks LCS ties toward deletions — locked in here because a
	// common-SUFFIX trim would reorder this to "-a +x  x" (which is why we only trim
	// the prefix).
	if got, want := stripANSI(lineDiff("a\nx\n", "x\nx\n")), "- a\n  x\n+ x\n"; got != want {
		t.Errorf("tie-break order changed:\ngot  %q\nwant %q", got, want)
	}
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func TestRenderNetworks(t *testing.T) {
	root := t.TempDir()
	tfDir := filepath.Join(root, "terraform-iac-bootstrap")
	aplDir := filepath.Join(root, "apl-values")

	lz, err := clusterspec.Decode([]byte(`
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
  networks:
    ord-shared: { region: us-ord }
    sea-shared: { region: us-sea }
`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	renderAndWrite(t, lz, nil, tfDir, aplDir, true)
	for name, region := range map[string]string{"ord-shared": "us-ord", "sea-shared": "us-sea"} {
		b, err := os.ReadFile(filepath.Join(tfDir, "vpc", name+".tfvars"))
		if err != nil {
			t.Fatalf("read vpc/%s.tfvars: %v", name, err)
		}
		for _, want := range []string{`vpc_label = "` + name + `"`, `region = "` + region + `"`} {
			if !strings.Contains(dealign(string(b)), want) {
				t.Errorf("vpc/%s.tfvars missing %q:\n%s", name, want, b)
			}
		}
	}

	// No networks declared → no vpc/<name>.tfvars target at all (no vpc root needed).
	lz2, _ := clusterspec.Decode([]byte("apiVersion: llz.akamai-consulting.io/v1alpha1\nkind: LandingZone\nmetadata: { name: i }\nspec:\n  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }\n"))
	targets, err := renderTargets(lz2, nil, tfDir, aplDir, true)
	if err != nil {
		t.Fatalf("renderTargets (no networks): %v", err)
	}
	for p := range targets {
		if strings.HasSuffix(p, ".tfvars") {
			t.Errorf("no networks/envs declared, but %s is a render target", p)
		}
	}
}

// listDeployments must also discover the split layout (landingzone.yaml +
// environments/*.yaml), unioned with committed tfvars.
func TestListDeployments_SplitSpecUnion(t *testing.T) {
	root := t.TempDir()
	tfDir := filepath.Join(root, "terraform-iac-bootstrap")
	writeCluster(t, tfDir, map[string]string{
		"legacy.tfvars": "region = \"us-sea\"\n",
	})
	mustWrite(t, filepath.Join(root, clusterspec.LandingZoneFile), `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: i }
spec:
  instance: { upstreamOrg: o, repo: o/i, forge: github, templateVersion: main }
`)
	if err := os.MkdirAll(filepath.Join(root, clusterspec.EnvironmentsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, clusterspec.EnvironmentsDir, "prod.yaml"), `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: prod }
spec:
  cluster:
    clusterLabel: platform-prod
    region: us-ord
    k8sVersion: v1.33.6+lke7
    nodePool: { type: g8-dedicated-8-4, count: 5 }
    bootstrap: { name: platform-prod }
    objectStorage: { cluster: us-ord-1 }
`)

	got, err := listDeployments(tfDir)
	if err != nil {
		t.Fatalf("listDeployments: %v", err)
	}
	if want := []string{"legacy", "prod"}; !reflect.DeepEqual(got, want) {
		t.Errorf("split union = %v, want %v", got, want)
	}
}
