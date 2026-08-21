package environments

// k8sversion_wiring_test.go — the gate for issue #448.
//
// THE ARCHETYPE IS "split contract" (docs/e2e-gates.md). `llz env add` PRODUCES
// cluster.k8sVersion; `llz ci assert-k8s-version` and `llz doctor` CONSUME it,
// both through linode.CheckVersion against the Linode ACCOUNT's catalog. Until
// #448 the two sides held incompatible copies of one rule — the producer a literal
// compiled into the scaffold months earlier, the consumer a live per-account list
// that rotates within hours (two accounts measured in the same hour on 2026-08-11
// offered disjoint versions). #443 made the consumer HARD-FAIL, so the
// disagreement stopped being a slow apply failure and became `llz up` refusing a
// pin the operator never chose.
//
// So this runs the REAL `llz env add` — resolver, spec writer and `llz render` —
// and feeds its REAL outputs to the REAL matcher. Nothing here restates the rule
// on either side, which is the property that makes it a coupling test rather than
// a second implementation that can agree with a bug.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envdef"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instanceresolve"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfvars"
)

// theOtherAccountCatalog is the account measured on 2026-08-11 still offering
// v1.33.6+lke7 while the e2e account offered only v1.34.6+lke2 and v1.32.9+lke4.
//
// THE FIXTURE IS CHOSEN SO THIS TEST CAN FAIL, and that choice is load-bearing.
// The scaffold's compiled-in default is v1.34.6+lke2, so against the E2E account's
// catalog it happens to be correct and every assertion below would pass with the
// resolver ripped out — "a coupling test that fixes the one input where two rules
// agree is a test of the test" (TestReaperRecognisesRelabelerOutput). Against THIS
// account the literal is exactly the pin #443 fails a build on, so a broken wiring
// is visible. The negative arm pins the fixture itself.
var theOtherAccountCatalog = []string{"v1.33.6+lke7"}

type fakeCatalog struct {
	versions []string
	calls    int
	// clusters is what /lke/clusters answers — the account's own record of which
	// deployments already exist, and the only witness to a re-scaffold once the
	// operator has deleted the spec (#453).
	clusters     []map[string]any
	clusterCalls int
}

func (f *fakeCatalog) ListClusters(context.Context) ([]map[string]any, error) {
	f.clusterCalls++
	return f.clusters, nil
}

// liveCluster is one row of the account's cluster listing. The label is the one
// envdef.ClusterLabelFor authors for these tests' instance (`platform-support`),
// which is the whole point: the lookup and the write must derive it once.
func liveCluster(env, region, version string) map[string]any {
	return map[string]any{
		"label":       envdef.ClusterLabelFor("platform-support", env),
		"region":      region,
		"k8s_version": version,
		// float64, WHICH IS WHAT encoding/json DECODES A NUMBER TO — the shape
		// linode.MatchClusterIDs is written against. Omitting it entirely was worse than
		// a wrong type: the orphan warning under test renders the id, so the fixture
		// produced "id 0 … delete that cluster" and the assertions below still passed.
		// A gate whose fixture cannot express the field its subject prints is one the
		// message can rot behind.
		"id": float64(4242),
	}
}

func (f *fakeCatalog) ListLKEVersions(_ context.Context, tier string) ([]string, error) {
	f.calls++
	if tier != linode.LKETierEnterprise {
		// LKE-E is the only product this landing zone builds; /v4/lke/versions
		// answers for a different one. See linode/lke_versions.go.
		return nil, context.Canceled
	}
	return f.versions, nil
}

// scaffoldWith runs a real `llz env add lab` in a throwaway instance root against
// the given catalog (nil = no account reachable) and returns the directory.
func scaffoldWith(t *testing.T, catalog *fakeCatalog, o envdef.Opts) (string, error) {
	t.Helper()
	return scaffoldEnv(t, t.TempDir(), "lab", catalog, o)
}

// scaffoldEnv is scaffoldWith against a NAMED directory and deployment, so a test
// can add a SECOND deployment to an instance the first call already scaffolded —
// which is the case where spec.defaults is inherited rather than written.
func scaffoldEnv(t *testing.T, dir, env string, catalog *fakeCatalog, o envdef.Opts) (string, error) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"),
		[]byte("upstream_org: akamai-consulting\ninstance_repo: my-org/platform-support\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	// THE OTHER TWO ACCOUNT CHECKS MUST NOT REACH THE NETWORK EITHER. Run still
	// calls CheckRegion and ResolveOBJCluster, and those build their own clients
	// straight from these variables — so on a developer machine with a token
	// exported, these tests would hit the live Linode API, fail on an account with
	// no us-ord-1, and pay a 20s timeout each against a slow one. Nothing about
	// k8sVersion would be under test any more.
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("LINODE_API_TOKEN", "")

	prev := instanceresolve.LKEVersionClient
	t.Cleanup(func() { instanceresolve.LKEVersionClient = prev })
	instanceresolve.LKEVersionClient = func() instanceresolve.LKEVersionLister {
		if catalog == nil {
			return nil
		}
		return catalog
	}
	return dir, Run(false, env, o)
}

// TestEnvAddSeedsAPinTheAccountCanActuallyBuild is the gate.
func TestEnvAddSeedsAPinTheAccountCanActuallyBuild(t *testing.T) {
	catalog := &fakeCatalog{versions: theOtherAccountCatalog}
	dir, err := scaffoldWith(t, catalog, envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	if err != nil {
		t.Fatalf("llz env add: %v", err)
	}
	if catalog.calls == 0 {
		// The wiring half. Everything below still passes if `llz env add` stops
		// asking and the literal happens to match; this is what says it asked.
		t.Error("`llz env add` never asked the account which LKE-Enterprise versions it can build")
	}

	// ── what `llz ci assert-k8s-version` and `llz doctor` will read ───────────
	lz, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatalf("LoadInstance: %v", err)
	}
	pin := strings.TrimSpace(lz.Spec.Defaults.Cluster.K8sVersion)
	// FAIL CLOSED ON VACUITY: linode.CheckVersion reads an empty pin as UNKNOWN, so
	// a scaffold that seeded nothing would be indistinguishable from one that seeded
	// correctly.
	if pin == "" {
		t.Fatal("the scaffold seeded no spec.defaults.cluster.k8sVersion — an empty pin " +
			"reads as UNCHECKED everywhere downstream, which is the state this gate exists to refuse")
	}
	if verdict, _ := linode.CheckVersion(pin, theOtherAccountCatalog); verdict != linode.VersionOffered {
		t.Errorf("`llz env add` seeded k8sVersion %q, which the preflight reads as %v against the very "+
			"account that scaffolded it (%v).\n`llz new` → `llz env add` → `llz up` must not stop at "+
			"`llz ci assert-k8s-version` on a pin the operator never chose — issue #448.",
			pin, verdict, theOtherAccountCatalog)
	}

	// ── and the string terraform is actually handed ───────────────────────────
	// The spec is reconciled into <env>.tfvars by `llz render`, and k8s_version goes
	// to the LKE create API VERBATIM. Asserting only the spec would miss a render
	// that drops or rewrites it on the way.
	b, err := os.ReadFile(filepath.Join(dir, "terraform-iac-bootstrap", "cluster", "lab.tfvars"))
	if err != nil {
		t.Fatalf("read the rendered cluster tfvars: %v", err)
	}
	rendered := strings.Trim(tfvars.Value(string(b), "k8s_version"), `"`)
	if rendered != pin {
		t.Errorf("cluster/lab.tfvars sends k8s_version = %q but the spec pins %q — terraform sends the "+
			"tfvars value, so the preflight would be checking a different string than the apply uses", rendered, pin)
	}
}

// TestARescaffoldOverALiveClusterPinsWhatItRuns is the gate for issue #453.
//
// THE ARCHETYPE IS THE SAME "split contract" as the gate above, with the producer
// asked a question it previously could not answer. `llz env add` cannot tell a
// RE-SCAFFOLD over a live cluster from a first run — the single-deployment case
// deletes landingzone.yaml, environments/<env>.yaml and the overlay together, so
// the tree is byte-for-byte a fresh instance and every disk-shaped guard is blind.
// Against a cluster that already exists, any answer other than the one it is
// running is an LKE-Enterprise control-plane upgrade nobody asked for, and
// `llz ci assert-k8s-version` cannot catch it: the new pin IS in the catalog, it is
// simply not the one that cluster runs.
//
// So this drives the REAL `llz env add` against a faked account holding a cluster
// at a version that has ROTATED OUT of that account's catalog — the shape that
// makes the two candidate answers maximally different — and reads the authored
// spec and the rendered tfvars, which is the string terraform actually sends.
func TestARescaffoldOverALiveClusterPinsWhatItRuns(t *testing.T) {
	// The account can build v1.34.6+lke2 / v1.32.9+lke4 today; the cluster is on
	// v1.33.6+lke7, which it can no longer build. Pre-#453 the scaffold seeded the
	// newest and planned an upgrade.
	const running = "v1.33.6+lke7"
	catalog := &fakeCatalog{
		versions: []string{"v1.34.6+lke2", "v1.32.9+lke4"},
		clusters: []map[string]any{liveCluster("lab", "us-ord", running)},
	}
	dir, err := scaffoldWith(t, catalog, envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	if err != nil {
		t.Fatalf("llz env add: %v", err)
	}
	if catalog.clusterCalls == 0 {
		// The wiring half. Every assertion below could be satisfied by a resolver
		// that never asked; this is what says `llz env add` asked the ACCOUNT.
		t.Fatal("`llz env add` never listed the account's clusters, so it still cannot tell a " +
			"re-scaffold over a live cluster from a first run — issue #453")
	}

	// LoadInstance folds spec.defaults into the deployment, so this is the pin
	// `llz ci assert-k8s-version` will read for "lab" — the consumer's own view.
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatalf("LoadInstance: %v", err)
	}
	e, ok := inst.Env("lab")
	if !ok {
		t.Fatal("the deployment was not authored")
	}
	if pin := strings.TrimSpace(e.Cluster.K8sVersion); pin != running {
		t.Errorf("`llz env add` pinned k8sVersion %q for a deployment whose cluster already runs %q.\n"+
			"terraform sends k8s_version on a create OR A CHANGE, so that plans an LKE-Enterprise "+
			"control-plane upgrade nobody asked for — and the preflight cannot catch it, because %[1]q "+
			"IS in the account's catalog. Issue #453.", pin, running)
	}
	// AND IT IS PER-DEPLOYMENT. spec.defaults must still be the account's newest: a
	// deployment added to this instance next quarter genuinely should get today's
	// version, not this one cluster's.
	if shared := strings.TrimSpace(inst.Spec.Defaults.Cluster.K8sVersion); shared != "v1.34.6+lke2" {
		t.Errorf("spec.defaults.cluster.k8sVersion = %q, want v1.34.6+lke2 — adopting one cluster's "+
			"running version must not move the shared default every later deployment inherits", shared)
	}

	// ── and the string terraform is actually handed ───────────────────────────
	b, err := os.ReadFile(filepath.Join(dir, "terraform-iac-bootstrap", "cluster", "lab.tfvars"))
	if err != nil {
		t.Fatalf("read the rendered cluster tfvars: %v", err)
	}
	if rendered := strings.Trim(tfvars.Value(string(b), "k8s_version"), `"`); rendered != running {
		t.Errorf("cluster/lab.tfvars sends k8s_version = %q but the cluster runs %q — terraform "+
			"compares the tfvars value against the API, so this plans the upgrade after all", rendered, running)
	}
}

// TestAFreshDeploymentStillGetsTheAccountsNewest is the negative arm, and without
// it "always pin what's running" passes the gate above while doing the wrong thing
// on every fresh instance — which is most of them.
//
// The fixture differs from the gate above in ONE fact: the account holds no cluster
// for this deployment. Everything else — catalog, region, flags — is identical.
func TestAFreshDeploymentStillGetsTheAccountsNewest(t *testing.T) {
	catalog := &fakeCatalog{
		versions: []string{"v1.34.6+lke2", "v1.32.9+lke4"},
		// A cluster for a DIFFERENT deployment on the same account, at a version this
		// test would notice being adopted. The match is label+region, not "any cluster".
		clusters: []map[string]any{liveCluster("dr", "us-ord", "v1.33.6+lke7")},
	}
	dir, err := scaffoldWith(t, catalog, envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	if err != nil {
		t.Fatalf("llz env add: %v", err)
	}
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatalf("LoadInstance: %v", err)
	}
	e, _ := inst.Env("lab")
	if pin := strings.TrimSpace(e.Cluster.K8sVersion); pin != "v1.34.6+lke2" {
		t.Errorf("a deployment with no cluster pinned %q, want v1.34.6+lke2 (the newest the account "+
			"offers). Adopting a version off an unrelated cluster is worse than the bug #453 fixes.", pin)
	}
	// AND IT DID NOT DIVERGE FROM THE SHARED DEFAULT: nothing to adopt means nothing
	// to override, so environments/lab.yaml carries no k8sVersion of its own.
	b, err := os.ReadFile(filepath.Join(dir, clusterspec.EnvironmentsDir, "lab.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "k8sVersion:") {
		t.Errorf("environments/lab.yaml pinned its own k8sVersion with no cluster to adopt one from:\n%s", b)
	}
}

// TestARescaffoldOverALiveClusterSaysSo — face 3 of #453.
//
// The re-seed guard (`reseeding`, add.go) needs ANOTHER environments/*.yaml to
// survive, so it catches "landingzone.yaml went missing" and not the
// single-deployment re-scaffold, where the spec, the env file and the overlay are
// removed together. Nothing on disk distinguishes the result from a fresh instance
// — the account does, and this asserts the operator is told.
func TestARescaffoldOverALiveClusterSaysSo(t *testing.T) {
	catalog := &fakeCatalog{
		versions: []string{"v1.34.6+lke2", "v1.32.9+lke4"},
		clusters: []map[string]any{liveCluster("lab", "us-ord", "v1.33.6+lke7")},
	}
	out := captureStderr(t, func() {
		if _, err := scaffoldWith(t, catalog, envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
			t.Fatalf("llz env add: %v", err)
		}
	})
	for _, want := range []string{
		"platform-support-lab", // which cluster it found
		"v1.33.6+lke7",         // and what that cluster runs
		"RE-CREATED",           // and the state the operator now carries
		"ORPHAN",               // and the one way this exemption is wrong
		"4242",                 // and the id the remedy tells them to act on
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scaffolding over a live cluster at a rotated-out version must say %q; stderr was:\n%s", want, out)
		}
	}
}

// TestDryRunOverALiveClusterPreviewsRatherThanReports.
//
// The adoption note/warning is a version consequence like the other two, so it goes
// through printK8sVersionConsequences — which both paths call — rather than being
// printed at the resolve site, where it landed ABOVE the "Spec that would be
// authored" header. A preview that opens with a past-tense report of a write is
// worse than no preview: it reads as though the run already happened.
func TestDryRunOverALiveClusterPreviewsRatherThanReports(t *testing.T) {
	dir := t.TempDir()
	catalog := &fakeCatalog{
		versions: []string{"v1.34.6+lke2", "v1.32.9+lke4"},
		clusters: []map[string]any{liveCluster("lab", "us-ord", "v1.33.6+lke7")},
	}
	var err error
	out := captureStderr(t, func() {
		_, err = scaffoldEnv(t, dir, "lab", catalog,
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1", DryRun: true})
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	// IT STILL DISCLOSES THE DECISION — the point is not to go quiet under --dry-run,
	// which would hide the one thing this preview is now uniquely able to show.
	if !strings.Contains(out, "v1.33.6+lke7") {
		t.Errorf("--dry-run did not preview that llz would pin the running version:\n%s", out)
	}
	if strings.Contains(out, "pinned it") || strings.Contains(out, "can no longer be") {
		t.Errorf("--dry-run reports a write that never happened:\n%s", out)
	}
	// AND IT REALLY WAS A DRY RUN.
	for _, p := range []string{clusterspec.LandingZoneFile, filepath.Join(clusterspec.EnvironmentsDir, "lab.yaml")} {
		if _, serr := os.Stat(filepath.Join(dir, p)); serr == nil {
			t.Errorf("--dry-run wrote %s", p)
		}
	}
}

// TestARenderRejectedSpecCanStillBeRecoveredWhenTheVersionHasRotated.
//
// An env file with NO overlay means a previous `llz env add` authored the spec and
// `llz render` then rejected it. That state used to dead-end — this refused, and
// `llz doctor` sent you back here — so the preflight grew a sentence naming the way
// out. Then the account reads were hoisted above it, and re-running with the
// original flags could die on the --k8s-version instead: LKE-E availability rotates
// within hours, so the pin that worked when the spec was authored is routinely gone
// by the time anyone comes back to fix the render error. The operator never reached
// the recovery sentence.
func TestARenderRejectedSpecCanStillBeRecoveredWhenTheVersionHasRotated(t *testing.T) {
	dir := t.TempDir()
	// The state a rejected render leaves: environments/lab.yaml with no overlay.
	if err := os.MkdirAll(filepath.Join(dir, clusterspec.EnvironmentsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, clusterspec.EnvironmentsDir, "lab.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The account can no longer build the pin the operator is re-running with.
	_, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: []string{"v1.34.6+lke2"}},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1", K8sVersion: "v1.33.6+lke7"})
	if err == nil {
		t.Fatal("expected `llz env add` to refuse — the env file already exists")
	}
	if !strings.Contains(err.Error(), "llz render lab") {
		t.Errorf("re-running after a rejected render died on the VERSION instead of naming the way out\n"+
			"of the dead-end the preflight exists to break. got:\n%s", err)
	}
}

// TestTheReSeedWarningDoesNotContradictTheLineAboveIt.
//
// printK8sVersionConsequences runs AFTER EnsureLandingZone on the write path, so a
// warning opening "<lzPath> is missing" printed two lines below "created <lzPath>"
// — a claim the run had just falsified itself. It is one string shared with the
// --dry-run path, where the file genuinely does not exist yet, so the fix is a
// tense that is true in both: llz observed it missing, and that is what it says.
func TestTheReSeedWarningDoesNotContradictTheLineAboveIt(t *testing.T) {
	dir := t.TempDir()
	catalog := []string{"v1.34.6+lke2"}
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: catalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, clusterspec.LandingZoneFile)); err != nil {
		t.Fatal(err)
	}
	var err error
	out := captureStderr(t, func() {
		_, err = scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: catalog},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if err != nil {
		t.Fatalf("second `llz env add`: %v", err)
	}
	if !strings.Contains(out, "RE-SEEDED") {
		t.Fatalf("the re-seed warning did not fire at all:\n%s", out)
	}
	// THE FILE EXISTS BY NOW — this run created it — so the present tense is false.
	if strings.Contains(out, "is missing") {
		t.Errorf("the warning says landingzone.yaml \"is missing\" on the path that just CREATED it:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(dir, clusterspec.LandingZoneFile)); serr != nil {
		t.Fatal("premise broken: the run did not re-create landingzone.yaml")
	}
}

// TestACatalogThatANSWEREDIsNotBlamedOnAMissingToken.
//
// An account whose catalog comes back naming no full build id is a THIRD state, and
// seedSource keyed on Newest alone collapsed it into "this account was never
// asked". In one run llz printed the catalog it had just read — in a warning — and
// then blamed the operator's credential for it a few lines later. The two sentences
// were about the same request.
func TestACatalogThatANSWEREDIsNotBlamedOnAMissingToken(t *testing.T) {
	// Coarse rows only: a real answer, with nothing in it terraform could send.
	coarse := &fakeCatalog{versions: []string{"1.34", "1.33"}}
	var err error
	out := captureStdout(t, func() {
		_, err = scaffoldWith(t, coarse, envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if err != nil {
		t.Fatalf("llz env add: %v", err)
	}
	if coarse.calls == 0 {
		t.Fatal("premise broken: the account was not asked at all")
	}
	if strings.Contains(out, "never asked") {
		t.Errorf("llz read this account's catalog and then said it never asked for it:\n%s", out)
	}
	if !strings.Contains(out, "names no full build id") {
		t.Errorf("the seed's provenance does not say the catalog answered but held nothing usable:\n%s", out)
	}
}

// TestAStarterExampleIsNotADeployment.
//
// existingDeployments filters environments/ to `.yaml`, and nothing pinned it:
// mutating the suffix check away left the whole suite green. What it defends is
// specific — the template tree ships `prod-web-ord.yaml.example` as a starter, and
// without the filter that file counts as a deployment. The re-seed warning then
// names a `.example` as something that will inherit the new pin, and `reseeding`
// flips true in this repo's own e2e lane, which rm's landingzone.yaml at the
// template root. A warning that names a file nobody deployed is how a real warning
// stops being read.
func TestAStarterExampleIsNotADeployment(t *testing.T) {
	dir := t.TempDir()
	envDir := filepath.Join(dir, clusterspec.EnvironmentsDir)
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"lab.yaml", "prod-web-ord.yaml.example", "README.md", "dr.yaml"} {
		if err := os.WriteFile(filepath.Join(envDir, f), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := existingDeployments(dir)
	want := []string{"dr", "lab"}
	if len(got) != len(want) {
		t.Fatalf("existingDeployments = %v, want %v — a starter `.example` (or a README) counted as a "+
			"deployment that inherits the re-seeded pin", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("existingDeployments = %v, want %v", got, want)
		}
	}
}

// TestTheThirdCatalogStateCHANGESWhatLlzSays.
//
// Round 9 split the catalog flag into three states because "no token" and "asked, and
// the API refused" licensed different sentences and different remedies. The
// resolver test pins that CatalogFailed is PRODUCED — and nothing pinned that any
// consumer says anything different about it, which is the entire user-visible point
// of the third state. Mutation probe: rewriting both CatalogFailed arms back to the
// CatalogNotAsked wording left the environments suite green.
//
// So this asserts the two consumers DISTINGUISH all three, without pinning their
// wording: what must hold is that a state llz can tell apart is one the operator can
// tell apart too.
func TestTheThirdCatalogStateCHANGESWhatLlzSays(t *testing.T) {
	states := map[string]instanceresolve.K8sVersionChoice{
		"not asked": {Catalog: instanceresolve.CatalogNotAsked},
		"failed":    {Catalog: instanceresolve.CatalogFailed},
		"answered":  {Catalog: instanceresolve.CatalogAnswered},
	}
	for _, render := range []struct {
		name string
		fn   func(instanceresolve.K8sVersionChoice) string
	}{
		{"k8sVersionBanner", func(k instanceresolve.K8sVersionChoice) string { return k8sVersionBanner(k, false, true, "", "", "") }},
		{"seedSource", seedSource},
	} {
		seen := map[string]string{}
		for state, k8s := range states {
			got := render.fn(k8s)
			if prior, dup := seen[got]; dup {
				t.Errorf("%s renders %q for BOTH the %q and %q catalog states — llz knows they are "+
					"different (one is 'export a token', the other is 'fix the token you have') and "+
					"the operator cannot tell", render.name, got, prior, state)
			}
			seen[got] = state
		}
	}
	// AND THE FAILED ARM POINTS AT THE DIAGNOSTIC THAT SAYS WHY, which is the whole
	// reason the state is worth distinguishing: the skip notice above it names the
	// status code.
	if src := seedSource(states["failed"]); !strings.Contains(src, "did not answer") {
		t.Errorf("seedSource for a FAILED read = %q, which does not tell the operator the account was "+
			"asked and refused", src)
	}
}

// TestTheE2ELaneScaffoldsBEFOREItRenamesTheInstance.
//
// THE WARM LANE'S WHOLE GUARANTEE RESTS ON A STEP ORDER NOTHING PINNED. `llz env
// add` derives this deployment's cluster label from envdef.InstanceName, which at
// the template root finds `.copier-answers.yml` unrendered and falls back to the
// directory name — `instance-template-e2e`. That is the label the surviving warm
// cluster carries, so the adoption fires and no control-plane upgrade is planned.
//
// `llz spec set instance.repo=` runs AFTER, and changes what InstanceName would
// return. Move it earlier and the label becomes the real repo's short name, the
// lookup matches nothing, and adoption stops — SILENTLY, because `Matches == 0` is
// the one classifyClusters outcome that deliberately emits no warning (it is the
// ordinary fresh-instance case; see TestAnAccountWithNoMatchIsQuiet). The lane
// would plan the upgrade this branch removed the documented backstop for, with
// nothing in the log to say so.
//
// So the order is load-bearing in a way neither step announces, and this is the
// only thing that would notice it changing.
func TestTheE2ELaneScaffoldsBEFOREItRenamesTheInstance(t *testing.T) {
	raw, err := os.ReadFile(e2eInstantiateWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	// `bin/llz`, NOT `llz` — the bare name appears in COMMENTS above both steps
	// (including one about E2E_K8S_VERSION 200 lines earlier), so the first cut of
	// this gate compared the positions of two sentences and passed no matter how the
	// commands were ordered. Caught by swapping the real steps and watching it stay
	// green: a gate whose mutation lands on prose is not a gate.
	body := string(raw)
	add := strings.Index(body, "bin/llz env add")
	rename := strings.Index(body, "bin/llz spec set instance.repo=")
	if add < 0 || rename < 0 {
		t.Fatalf("premise broken: `bin/llz env add` at %d, `bin/llz spec set instance.repo=` at %d — "+
			"this gate is watching commands that are no longer in the lane", add, rename)
	}
	if add > rename {
		t.Error("`llz spec set instance.repo=` now runs BEFORE `llz env add`, so the cluster label the\n" +
			"scaffold looks up is no longer the one the warm cluster carries. Adoption stops matching\n" +
			"and says nothing — `Matches == 0` is the one outcome with no warning — and the warm lane\n" +
			"plans an LKE-Enterprise control-plane upgrade it used to be pinned against.")
	}
}

// TestEnvAddFallsBackToTheScaffoldDefaultWithNoAccount is the fail-OPEN half AND
// the negative arm that keeps the fixture above honest.
//
// `llz env add` has never needed a Linode token or a network and must not start.
// It also proves the compiled default and the account-derived pin are DIFFERENT
// strings for this catalog — without that, the test above would pass with the
// resolver unwired and prove nothing.
func TestEnvAddFallsBackToTheScaffoldDefaultWithNoAccount(t *testing.T) {
	dir, err := scaffoldWith(t, nil, envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	if err != nil {
		t.Fatalf("`llz env add` must still work with no LINODE_TOKEN: %v", err)
	}
	lz, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatalf("LoadInstance: %v", err)
	}
	baked := strings.TrimSpace(lz.Spec.Defaults.Cluster.K8sVersion)
	if baked == "" {
		t.Fatal("the offline fallback must still author a spec that validates")
	}
	if verdict, _ := linode.CheckVersion(baked, theOtherAccountCatalog); verdict != linode.VersionNotOffered {
		t.Errorf("the compiled scaffold default %q reads as %v against %v. It must be NotOffered, or "+
			"TestEnvAddSeedsAPinTheAccountCanActuallyBuild passes whether or not the resolver is wired.",
			baked, verdict, theOtherAccountCatalog)
	}
}

// TestEnvAddRefusesAnExplicitPinTheAccountCannotBuild — the one arm that FAILS.
// The neighbours (CheckRegion, ResolveOBJCluster) already take this licence, and
// the alternative is authoring a spec the operator must then be told to fix.
func TestEnvAddRefusesAnExplicitPinTheAccountCannotBuild(t *testing.T) {
	dir, err := scaffoldWith(t, &fakeCatalog{versions: theOtherAccountCatalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1", K8sVersion: "v1.34.6+lke2"})
	if err == nil {
		t.Fatal("`llz env add --k8s-version` must reject a version the account cannot build")
	}
	for _, want := range []string{"v1.34.6+lke2", "v1.33.6+lke7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure must name both the rejected pin and what the account DOES offer; got:\n%s", err)
		}
	}
	// Nothing authored: a refused pin must not leave a half-scaffolded deployment
	// that the next `llz env add` then refuses to overwrite.
	if _, serr := os.Stat(filepath.Join(dir, clusterspec.LandingZoneFile)); serr == nil {
		t.Error("a rejected --k8s-version wrote landingzone.yaml anyway, which dead-ends the retry")
	}
}

// TestALaterDeploymentIsNotCreatedAgainstARotatedOutSharedPin is the second half
// of the gate, and it exists because the first cut of this feature did not cover
// it.
//
// spec.defaults.cluster.k8sVersion is seeded ONCE, on the first `llz env add`.
// Every deployment added afterwards — a second region, a DR peer, a deployment
// added a quarter later — INHERITS it, and nothing re-checked it in between. LKE-E
// availability rotates within hours, so the shared pin can be one the account can
// no longer build: the new deployment is created against it and dies in exactly
// the way #443 gates and #448 exists to prevent, one deployment along.
//
// Worse, the command PRINTED "derived" about the version it then discarded, which
// is the "says something true and useless" failure docs/e2e-gates.md warns about
// — it looked like it had done the thing.
func TestALaterDeploymentIsNotCreatedAgainstARotatedOutSharedPin(t *testing.T) {
	dir := t.TempDir()

	// Deployment 1 scaffolds the instance against an account offering v1.33.6+lke7,
	// so spec.defaults carries it. Then that version leaves the catalog.
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: theOtherAccountCatalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	lz, err := clusterspec.Load(filepath.Join(dir, clusterspec.LandingZoneFile))
	if err != nil {
		t.Fatal(err)
	}
	shared := strings.TrimSpace(lz.Spec.Defaults.Cluster.K8sVersion)
	if shared != "v1.33.6+lke7" {
		t.Fatalf("spec.defaults seeded %q, want v1.33.6+lke7 — this test's premise", shared)
	}

	// Deployment 2, added later, against a catalog the shared pin has rotated out of.
	rotated := []string{"v1.34.6+lke2", "v1.32.9+lke4"}
	if _, err := scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: rotated},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("second `llz env add`: %v", err)
	}

	// LoadInstance folds spec.defaults into every deployment, so this is the pin
	// `llz ci assert-k8s-version` will read for "dr" — the consumer's own view.
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := inst.Env("dr")
	if !ok {
		t.Fatal("the second deployment was not authored")
	}
	pin := strings.TrimSpace(e.Cluster.K8sVersion)
	if verdict, _ := linode.CheckVersion(pin, rotated); verdict != linode.VersionOffered {
		t.Errorf("the second deployment pins k8sVersion %q, which the preflight reads as %v against %v.\n"+
			"It inherited spec.defaults (%q) unchecked — the failure #448 removes, one deployment along.",
			pin, verdict, rotated, shared)
	}

	// AND THE FIRST DEPLOYMENT IS UNTOUCHED. It already runs its pin; terraform
	// plans no change to k8s_version for it (linode.ClusterRunsVersion), so
	// rewriting the shared default would force a control-plane upgrade nobody asked
	// for on every existing deployment.
	if first, _ := inst.Env("lab"); strings.TrimSpace(first.Cluster.K8sVersion) != shared {
		t.Errorf("the existing deployment moved from %q to %q — adding a deployment must not "+
			"upgrade the control plane of the ones already running", shared, first.Cluster.K8sVersion)
	}
}

// TestALaterDeploymentInheritsWhenTheSharedPinIsStillBuildable is the negative arm
// of the test above: divergence is a real cost and may only be imposed on
// evidence. Without this, "always override" would pass the test above and quietly
// give every deployment its own pin.
func TestALaterDeploymentInheritsWhenTheSharedPinIsStillBuildable(t *testing.T) {
	dir := t.TempDir()
	catalog := []string{"v1.34.6+lke2", "v1.33.6+lke7"}
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: catalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	if _, err := scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: catalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("second `llz env add`: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, clusterspec.EnvironmentsDir, "dr.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "k8sVersion:") {
		t.Errorf("environments/dr.yaml pinned its own k8sVersion while the shared default is still "+
			"buildable — deployments must not drift apart for no reason:\n%s", b)
	}
}

// TestAnExplicitPinDoesNotBecomeTheSharedDefault — `--k8s-version` is
// per-deployment, like --region and --node-type. Seeding spec.defaults from it let
// one deliberately-pinned first deployment silently decide the version of every
// deployment added afterwards.
func TestAnExplicitPinDoesNotBecomeTheSharedDefault(t *testing.T) {
	catalog := []string{"v1.34.6+lke2", "v1.32.9+lke4"}
	dir, err := scaffoldWith(t, &fakeCatalog{versions: catalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1", K8sVersion: "v1.32.9+lke4"})
	if err != nil {
		t.Fatalf("llz env add: %v", err)
	}
	lz, err := clusterspec.Load(filepath.Join(dir, clusterspec.LandingZoneFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(lz.Spec.Defaults.Cluster.K8sVersion); got != "v1.34.6+lke2" {
		t.Errorf("spec.defaults.cluster.k8sVersion = %q, want the account's newest (v1.34.6+lke2). "+
			"An explicit --k8s-version pins ONE deployment; it must not decide the instance default.", got)
	}
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := inst.Env("lab"); strings.TrimSpace(e.Cluster.K8sVersion) != "v1.32.9+lke4" {
		t.Errorf("the pinned deployment got %q, want the --k8s-version it was given", e.Cluster.K8sVersion)
	}
}

// TestK8sVersionBannerTellsTheOperatorWhichFileDecides — the banner line is where
// an operator reads what their cluster's version will be, and the four states are
// genuinely different answers. Collapsing any two is what let a second `env add`
// announce a version it did not use.
func TestK8sVersionBannerTellsTheOperatorWhichFileDecides(t *testing.T) {
	catalog := []string{"v1.34.6+lke2"}
	for name, c := range map[string]struct {
		choice        instanceresolve.K8sVersionChoice
		lzExists      bool
		inheritedFix  string
		inherited     string // what landingzone.yaml's spec.defaults holds
		lzUnreadable  bool   // the spec did not parse (zero value = it did)
		missingPinFix string
		want          string
	}{
		"explicit pin wins": {
			instanceresolve.K8sVersionChoice{Pin: "v1.32.9+lke4", Newest: "v1.34.6+lke2", Offered: catalog, Catalog: instanceresolve.CatalogAnswered},
			false, "", "", false, "", "v1.32.9+lke4",
		},
		"first env add shows the derived version": {
			instanceresolve.K8sVersionChoice{Newest: "v1.34.6+lke2", Offered: catalog, Catalog: instanceresolve.CatalogAnswered}, false, "", "", false, "", "v1.34.6+lke2",
		},
		"later env add inherits": {
			instanceresolve.K8sVersionChoice{Newest: "v1.34.6+lke2", Offered: catalog, Catalog: instanceresolve.CatalogAnswered}, true, "", "v1.33.6+lke7", false, "", "inherited",
		},
		"later env add overrides a rotated-out shared pin": {
			instanceresolve.K8sVersionChoice{Newest: "v1.34.6+lke2", Offered: catalog, Catalog: instanceresolve.CatalogAnswered}, true, "v1.34.6+lke2", "v1.33.6+lke7", false, "", "this deployment only",
		},
		// THE TWO STATES THAT USED TO RENDER IDENTICALLY. One never reached the
		// account; the other got an answer it cannot use — and the second one prints
		// the catalog to stderr moments earlier, so "could not be asked" contradicted
		// a message already on screen.
		"account unreachable": {
			instanceresolve.K8sVersionChoice{}, false, "", "", false, "", "could not be asked",
		},
		"catalog answered but names no build id": {
			instanceresolve.K8sVersionChoice{Offered: []string{"1.33", "1.34"}, Catalog: instanceresolve.CatalogAnswered}, false, "", "", false, "", "names no build id",
		},
		// THE FIXTURE THAT CAUGHT THE DRIFT. A read that SUCCEEDED and returned an
		// empty catalog leaves Offered nil while Catalog is CatalogAnswered — so a banner
		// keyed on len(Offered) said "could not be asked" while seedSource, keyed on
		// Catalog, said the catalog had answered. Same run, same request, opposite
		// claims. Every fixture above now carries Catalog because the resolver
		// always sets the two together; a hand-built choice that omits it is a state
		// ResolveK8sVersion cannot produce, and it hid this.
		"catalog answered with nothing at all": {
			instanceresolve.K8sVersionChoice{Catalog: instanceresolve.CatalogAnswered}, false, "", "", false, "", "names no build id",
		},
		// TWO STATES THAT USED TO RENDER AS AN INHERITANCE. sharedK8sVersion folds "the
		// field is absent" and "the spec did not parse" into one "", and neither is an
		// inheritance. The absent-field case is not asserted HERE because it cannot
		// reach this function: with no shared pin and no --k8s-version, missingPinFix
		// is always set and its arm wins, so the only way into an `inherited == ""`
		// arm was a fixture Run cannot produce.
		// TestADeploymentIsNotAuthoredAgainstNoVersionWhenTheAccountAnswered covers the
		// reachable version of it end to end.
		"landingzone.yaml did not parse": {
			instanceresolve.K8sVersionChoice{Newest: "v1.34.6+lke2", Offered: catalog, Catalog: instanceresolve.CatalogAnswered},
			true, "", "", true, "", "could not be read",
		},
		"no shared default, so the account's answer is used here": {
			instanceresolve.K8sVersionChoice{Newest: "v1.34.6+lke2", Offered: catalog, Catalog: instanceresolve.CatalogAnswered},
			true, "", "", false, "v1.34.6+lke2", "this deployment only",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := k8sVersionBanner(c.choice, c.lzExists, !c.lzUnreadable, c.inherited, c.inheritedFix, c.missingPinFix)
			if !strings.Contains(got, c.want) {
				t.Errorf("banner = %q, want it to contain %q", got, c.want)
			}
		})
	}
	// And the two "no build id" states must not render the same string, which is the
	// whole finding rather than a wording preference.
	unreachable := k8sVersionBanner(instanceresolve.K8sVersionChoice{}, false, true, "", "", "")
	coarse := k8sVersionBanner(instanceresolve.K8sVersionChoice{Offered: []string{"1.33"}, Catalog: instanceresolve.CatalogAnswered}, false, true, "", "", "")
	if unreachable == coarse {
		t.Errorf("an unreachable account and a catalog llz cannot use both render %q — "+
			"they are different events with different remedies", unreachable)
	}
	// AND THE BANNER AND THE SEED PROVENANCE MUST NOT DISAGREE ABOUT ONE REQUEST.
	// They are two sentences in one run about whether the account answered; keyed on
	// different fields they drifted, and only a successful EMPTY read separates the
	// fields. Asserted rather than commented, because they live in different
	// functions and nothing else couples them.
	for name, k8s := range map[string]instanceresolve.K8sVersionChoice{
		"empty read":  {Catalog: instanceresolve.CatalogAnswered},
		"coarse read": {Offered: []string{"1.33"}, Catalog: instanceresolve.CatalogAnswered},
		"no read":     {},
	} {
		banner, source := k8sVersionBanner(k8s, false, true, "", "", ""), seedSource(k8s)
		if strings.Contains(banner, "could not be asked") != strings.Contains(source, "never asked") {
			t.Errorf("%s: the banner says %q and the seed provenance says %q — one run, one request, "+
				"two answers about whether the account was asked", name, banner, source)
		}
	}
}

// ── the repo's OWN e2e lane ──────────────────────────────────────────────────
//
// Everything above proves `llz env add` asks the account when a token is in
// scope. In release-e2e it was not: the scaffold runs in the TEMPLATE repo, while
// LINODE_API_TOKEN lives only in the instance repo's infra-e2e Environment — so
// the lane silently kept the compiled literal this PR exists to delete, and would
// go red at `llz ci assert-k8s-version` the day that literal rotates out of the
// e2e account. Exactly #426, which cost a release-e2e round.
//
// THE FAILURE MODE IS SILENCE, which is why this is gated rather than just fixed.
// Dropping the secret from any link in the chain does not error anywhere: the
// scaffold step simply stops asking and falls back, and a run that never asked
// looks identical to one that did.

const (
	workflowDir            = "../../../../../.github/workflows"
	e2eInstantiateWorkflow = workflowDir + "/e2e-instantiate.yml"
)

// e2eScaffoldStep returns the body of the `Scaffold the e2e environment` step.
//
// SCOPED TO THE STEP, NOT THE FILE, for the reason delivered_preflight_test.go
// records: this workflow's comments name both LINODE_TOKEN and E2E_LINODE_TOKEN
// several times, so a whole-file search would be satisfied by the prose with the
// assignment deleted — the gate would go vacuous from an edit that looks like
// documentation.
func e2eScaffoldStep(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(e2eInstantiateWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", e2eInstantiateWorkflow, err)
	}
	const marker = "      - name: Scaffold the e2e environment\n"
	i := strings.Index(string(raw), marker)
	if i < 0 {
		t.Fatalf("no `Scaffold the e2e environment` step in %s — the step this couples to has been "+
			"renamed or removed, and this test would pass having read nothing", e2eInstantiateWorkflow)
	}
	rest := string(raw)[i+len(marker):]
	if end := strings.Index(rest, "\n      - name: "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// TestTheE2ELaneScaffoldsAgainstTheAccountItBuildsOn is the gate.
func TestTheE2ELaneScaffoldsAgainstTheAccountItBuildsOn(t *testing.T) {
	step := e2eScaffoldStep(t)

	// FAIL CLOSED ON VACUITY: if this step no longer runs the scaffolder, the
	// assertion below is about a step that scaffolds nothing.
	if !strings.Contains(step, "llz env add") {
		t.Fatalf("the `Scaffold the e2e environment` step no longer runs `llz env add` — "+
			"whatever this test then asserts about its env block is meaningless:\n%s", step)
	}
	// The assignment, with its indentation, so `E2E_LINODE_TOKEN: ${{ ... }}` in the
	// Preconditions step cannot satisfy it by being a superstring.
	if !strings.Contains(step, "\n          LINODE_TOKEN: ${{ secrets.E2E_LINODE_TOKEN }}\n") {
		t.Errorf("the e2e scaffold step does not put a Linode token in scope, so `llz env add` cannot ask "+
			"the account which LKE-Enterprise versions it can build and falls back to the compiled default.\n"+
			"That default is stale by construction — availability is per-account and rotates within hours — "+
			"so release-e2e keeps the exact rot #448 removes for adopters, until it goes red at "+
			"`llz ci assert-k8s-version`.\nStep body:\n%s", step)
	}

	// The secret has to be DECLARED on the reusable workflow, or it is never in
	// scope no matter what the caller passes.
	raw, err := os.ReadFile(e2eInstantiateWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "      E2E_LINODE_TOKEN:\n") {
		t.Error("E2E_LINODE_TOKEN is used but not declared in the workflow_call `secrets:` block — " +
			"an undeclared secret is silently empty inside a reusable workflow")
	}
	// …and it must stay OPTIONAL. Requiring it breaks every fork's e2e on the commit
	// that adds it, for a check that is an improvement rather than a prerequisite.
	if !strings.Contains(string(raw), "'Read-only Linode PAT for the account that builds the e2e cluster. Optional; without it the scaffold falls back to the compiled k8sVersion default.'\n        required: false") {
		t.Error("E2E_LINODE_TOKEN must stay `required: false` — `llz env add` degrades without it, and a " +
			"required secret turns an improvement into a breaking change for every fork")
	}

	// ── every caller, ENUMERATED rather than named ───────────────────────────
	//
	// A reusable workflow only receives a secret its caller passes (or inherits),
	// and this repo has THREE call sites — the cold lane, the warm lane, and the
	// driver. Listing them here would have been the bug: the first cut of this test
	// named the warm caller alone and passed while release-e2e-lane.yml, the ACTUAL
	// cold path, still dropped the secret. So the callers are discovered, and a new
	// one is covered the moment it is added.
	callers := e2eInstantiateCallers(t)
	if len(callers) == 0 {
		t.Fatal("found no workflow calling e2e-instantiate.yml — this test would pass having checked nothing")
	}
	for name, body := range callers {
		// `secrets: inherit` forwards everything, so it needs no explicit line —
		// matched as a REAL YAML LINE, never as text. The first cut used
		// strings.Contains over the whole file and was satisfied by a COMMENT in
		// release-e2e-warm.yml that merely mentions `secrets: inherit`, so that
		// caller was skipped entirely and passed by luck. Same trap
		// delivered_preflight_test.go records from the other direction.
		if inheritsSecrets.MatchString(body) {
			continue
		}
		if !strings.Contains(body, "E2E_LINODE_TOKEN: ${{ secrets.E2E_LINODE_TOKEN }}") {
			t.Errorf("%s calls e2e-instantiate.yml with an explicit `secrets:` block that omits "+
				"E2E_LINODE_TOKEN. An unlisted secret is simply absent inside a reusable workflow, so this "+
				"lane would scaffold against the compiled default while the others ask the account — two "+
				"lanes building different clusters from the same commit.", name)
		}
	}
}

// inheritsSecrets matches a real `secrets: inherit` mapping line, never the phrase
// inside a comment.
var inheritsSecrets = regexp.MustCompile(`(?m)^\s*secrets: inherit\s*$`)

// callsInstantiate matches the `uses:` line, so a comment naming the workflow does
// not enrol a file that never calls it.
var callsInstantiate = regexp.MustCompile(`(?m)^\s*uses: \./\.github/workflows/e2e-instantiate\.yml\s*$`)

// nextJobOrTopLevelKey matches the start of the next job (2-space key) or a new
// top-level key — the end of one call site.
var nextJobOrTopLevelKey = regexp.MustCompile(`(?m)^(  )?[a-zA-Z_-]+:`)

// e2eInstantiateCallers returns each workflow's CALL SITE — the `uses:` line and
// the `with:`/`secrets:` block under it — keyed by file name.
//
// THE CALL SITE, NOT THE FILE. Whether a secret reaches the reusable workflow is
// decided by the block under this one `uses:`; a `secrets: inherit` belonging to
// some other job in the same file says nothing about it. Reading whole files also
// let one caller's prose answer for its own wiring.
func e2eInstantiateCallers(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowDir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") || e.Name() == "e2e-instantiate.yml" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(workflowDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		loc := callsInstantiate.FindStringIndex(string(raw))
		if loc == nil {
			continue
		}
		rest := string(raw)[loc[1]:]
		if end := nextJobOrTopLevelKey.FindStringIndex(rest); end != nil {
			rest = rest[:end[0]]
		}
		out[e.Name()] = rest
	}
	return out
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	os.Stderr = prev
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// captureStdout is captureStderr for the other stream. The two version decisions
// `llz env add` makes are announced on DIFFERENT streams on purpose — a warning is
// a consequence, a preview line is information — so a gate that watches only one
// of them watches half the output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	os.Stdout = prev
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// TestDryRunDisclosesTheSharedDefaultItWouldSeed.
//
// THE BANNER AND spec.defaults ANSWER DIFFERENT QUESTIONS, and with an explicit
// --k8s-version they hold different values: the flag is per-deployment, while
// spec.defaults is seeded from the account's NEWEST and is what every deployment
// added afterwards inherits. `--dry-run` showed the first and said nothing at all
// about the second — so the value with the longest reach was the one the preview
// omitted, which is the opposite of what a preview is for.
func TestDryRunDisclosesTheSharedDefaultItWouldSeed(t *testing.T) {
	// The pin is offered but is NOT the newest, so "the preview just echoes the
	// banner" cannot pass this.
	catalog := &fakeCatalog{versions: []string{"v1.34.6+lke2", "v1.32.9+lke4"}}
	var err error
	out := captureStdout(t, func() {
		_, err = scaffoldWith(t, catalog, envdef.Opts{
			Region: "us-ord", ObjCluster: "us-ord-1", K8sVersion: "v1.32.9+lke4", DryRun: true,
		})
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out, "v1.34.6+lke2") {
		t.Errorf("--dry-run never named the k8sVersion it would seed into spec.defaults (v1.34.6+lke2), "+
			"which every deployment added later inherits. got:\n%s", out)
	}
	if !strings.Contains(out, "inherited by every deployment") {
		t.Errorf("--dry-run named a version without saying it is the SHARED default, which is the "+
			"whole difference from the per-deployment pin in the banner. got:\n%s", out)
	}
}

// TestTheSeedIsDisclosedWithNoAccountToo is the arm the guard used to skip.
//
// The disclosure was written as `if k8s.Newest != ""`, which reads as "only say
// something when we derived something" and silences precisely the operator who
// most needs to hear it: with no LINODE_TOKEN the seed falls through to a literal
// compiled months ago, and that literal becomes every deployment's shared default.
func TestTheSeedIsDisclosedWithNoAccountToo(t *testing.T) {
	var err error
	out := captureStdout(t, func() {
		_, err = scaffoldWith(t, nil, envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if err != nil {
		t.Fatalf("llz env add: %v", err)
	}
	// THROUGH envdef.SeedK8sVersion, not a literal restated here: the gate must read
	// the same function the write does, or it pins a copy that goes stale on its own.
	if !strings.Contains(out, envdef.SeedK8sVersion("")) {
		t.Errorf("`llz env add` seeded spec.defaults from a compiled default (%s) without saying so. got:\n%s",
			envdef.SeedK8sVersion(""), out)
	}
	if !strings.Contains(out, "this account was never asked") {
		t.Errorf("the operator cannot tell a derived pin from a compiled one, which earn very "+
			"different reactions. got:\n%s", out)
	}
}

// TestReSeedingAMissingLandingZoneSaysWhoInheritsTheNewPin.
//
// THE ONE PATH THAT BYPASSES EVERY OTHER GUARD HERE. An absent landingzone.yaml
// reads as "fresh instance" and is re-seeded from today's catalog — but when
// environments/ still holds deployments, the file was DELETED rather than never
// written (this repo's own e2e lane `rm`s it every run, and add.go's start-over
// hint invites the same), and those deployments now inherit a version nobody chose
// for them. Completing an HA pair re-renders EVERY env, so the next apply plans a
// control-plane upgrade on clusters that are already running — exactly what
// TestALaterDeploymentIsNotCreatedAgainstARotatedOutSharedPin forbids on the
// inherit path.
//
// It WARNS rather than refusing, and the test pins that too: the pin those
// deployments used to inherit died with the file, so there is nothing to restore
// and no honest way to pick a replacement. Refusing would only block the recovery
// the operator is in the middle of.
func TestReSeedingAMissingLandingZoneSaysWhoInheritsTheNewPin(t *testing.T) {
	dir := t.TempDir()
	catalog := []string{"v1.34.6+lke2"}
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: catalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	// The instance loses landingzone.yaml while `lab` stays defined.
	if err := os.Remove(filepath.Join(dir, clusterspec.LandingZoneFile)); err != nil {
		t.Fatal(err)
	}

	var runErr error
	out := captureStderr(t, func() {
		_, runErr = scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: catalog},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if runErr != nil {
		t.Fatalf("re-seeding must not refuse — it is the recovery the operator is mid-way through: %v", runErr)
	}
	if !strings.Contains(out, "lab") {
		t.Errorf("the re-seed warning must NAME the deployments that silently inherit the new pin — "+
			"'some deployments may be affected' is the message that gets skimmed. got:\n%s", out)
	}
	for _, want := range []string{"RE-SEEDED", "v1.34.6+lke2", "control-plane upgrade"} {
		if !strings.Contains(out, want) {
			t.Errorf("the re-seed warning must mention %q; got:\n%s", want, out)
		}
	}
	// A NORMAL first `env add` must stay quiet, or the warning is noise that trains
	// the operator to ignore it.
	clean := t.TempDir()
	quiet := captureStderr(t, func() {
		if _, err := scaffoldEnv(t, clean, "lab", &fakeCatalog{versions: catalog},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
			t.Fatalf("clean scaffold: %v", err)
		}
	})
	if strings.Contains(quiet, "RE-SEEDED") {
		t.Errorf("a genuinely fresh instance must not get the re-seed warning:\n%s", quiet)
	}
}

// TestReSeedingWarnsWithNoTokenToo.
//
// THE SILENT HALF OF THE RE-SEED. Without a LINODE_TOKEN there is no catalog to
// derive from, but EnsureLandingZone still writes landingzone.yaml — it falls
// through to llz's compiled default — so the orphaned deployments inherit a pin
// nobody chose just as surely as on the derived path. Gating the warning on "did
// we derive something" made this exact case, the operator recovering a deleted
// spec on a laptop, the one that got no warning at all.
func TestReSeedingWarnsWithNoTokenToo(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldEnv(t, dir, "lab", nil,
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, clusterspec.LandingZoneFile)); err != nil {
		t.Fatal(err)
	}

	var runErr error
	out := captureStderr(t, func() {
		_, runErr = scaffoldEnv(t, dir, "dr", nil,
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if runErr != nil {
		t.Fatalf("re-seeding must not refuse: %v", runErr)
	}
	for _, want := range []string{"RE-SEEDED", "lab", "control-plane upgrade", envdef.SeedK8sVersion("")} {
		if !strings.Contains(out, want) {
			t.Errorf("the no-token re-seed warning must mention %q — the re-seed happened anyway; got:\n%s", want, out)
		}
	}
	// And it must name the version's PROVENANCE, because a compiled literal and a
	// version the account just offered warrant different reactions.
	if !strings.Contains(out, "compiled default") {
		t.Errorf("the no-token warning must say the pin is llz's compiled default, not an account answer; got:\n%s", out)
	}
	// The pin it names must be the one actually written, or the warning sends the
	// operator looking for a version that is nowhere on disk.
	lz, err := os.ReadFile(filepath.Join(dir, clusterspec.LandingZoneFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lz), envdef.SeedK8sVersion("")) {
		t.Errorf("the re-seeded spec must carry the version the warning named; got:\n%s", lz)
	}
}

// TestDryRunPreviewsTheVersionConsequencesItWouldCause.
//
// `--dry-run` exists to answer "what would this do", and the two things this
// command can do to a cluster's version — diverge THIS deployment from the shared
// pin, or re-seed a default every OTHER deployment inherits — were reported only
// on the path that performed them. The facts were read early specifically so the
// preview could show them, and then the preview returned first.
func TestDryRunPreviewsTheVersionConsequencesItWouldCause(t *testing.T) {
	dir := t.TempDir()
	catalog := []string{"v1.34.6+lke2"}
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: catalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, clusterspec.LandingZoneFile)); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStderr(t, func() {
		_, err = scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: catalog},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1", DryRun: true})
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out, "RE-SEEDED") || !strings.Contains(out, "lab") {
		t.Errorf("--dry-run did not preview the re-seed that would move `lab`'s inherited pin — "+
			"the state most worth previewing was visible only by doing it. got:\n%s", out)
	}
	// AND IT REALLY WAS A DRY RUN: previewing must not create the file it warns about.
	if _, serr := os.Stat(filepath.Join(dir, clusterspec.LandingZoneFile)); serr == nil {
		t.Error("--dry-run re-created landingzone.yaml")
	}
}

// TestTheE2ELaneCanStillPinTheVersionByHand.
//
// `k8s_version` reaches the LKE-E API on a create OR A CHANGE, and release-e2e-warm
// refreshes a LIVE cluster on purpose (it exists to skip the ~14m create), as does
// release-e2e with keep_cluster=true. A re-scaffold that lands on a different
// version than the running cluster plans a CONTROL-PLANE UPGRADE inside a run whose
// whole point was not to rebuild anything, and `assert-k8s-version` cannot catch it:
// the new pin IS in the catalog, it is simply not the one that cluster runs.
//
// #453 CLOSED THAT IN THE SCAFFOLD, WHICH IS WHY THIS TEST'S REASON CHANGED. With
// E2E_LINODE_TOKEN in scope `llz env add` asks the account whether a cluster for
// this deployment exists and pins WHAT IT RUNS, so the warm lane needs no var at
// all. What remains is the manual escape for the paths llz cannot ask about — no
// token, or a cluster read that failed — and it must stay reachable without editing
// the workflow.
//
// THE OLD RATIONALE SURVIVED THE FIX AND WAS THE LAST PLACE IN THE REPO CARRYING
// IT, after this branch rewrote both workflow headers and docs/secrets.md. A
// maintainer tripping this gate was told to set vars.E2E_K8S_VERSION — which the
// same branch now documents as hard-failing the next COLD run, once that version
// rotates out and an explicit pin has no cluster to exempt it.
func TestTheE2ELaneCanStillPinTheVersionByHand(t *testing.T) {
	step := e2eScaffoldStep(t)
	if !strings.Contains(step, `--k8s-version "${E2E_K8S_VERSION}"`) {
		t.Errorf("the e2e scaffold can no longer be pinned by hand, so the paths llz cannot ask about —\n"+
			"no E2E_LINODE_TOKEN, or a failed cluster read — have no way to stop a reused cluster being\n"+
			"re-derived onto a different version. Step body:\n%s", step)
	}
	raw, err := os.ReadFile(e2eInstantiateWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	// From a repo VAR, so the escape is reachable without a commit — and it must stay
	// optional, since EMPTY is now the normal setting: it is what lets `llz env add`
	// decide, adopting a live cluster's version or deriving the account's newest.
	if !strings.Contains(string(raw), "E2E_K8S_VERSION: ${{ vars.E2E_K8S_VERSION }}") {
		t.Error("E2E_K8S_VERSION must come from a repo var — a pin that needs a commit is one nobody sets")
	}
	// NO CONDITIONAL AROUND THE FLAG. `llz env add` trims and ignores an empty
	// --k8s-version, which is what keeps this out of the untestable-loc budget; a
	// shell `if` here would be charged for and would be untested.
	if strings.Contains(step, "if [[ -z \"${E2E_K8S_VERSION") {
		t.Error("the empty case is handled by `llz env add` itself; a shell conditional here is " +
			"untestable inline bash the budget gate charges for")
	}
}

// TestTheSeedSourceDoesNotClaimAnAccountWasNeverAsked — the consumer half.
//
// seedSource explains, in the preview and in the re-seed warning, WHERE the version
// llz just wrote came from. It keyed the three-way split on len(k8s.Offered), which
// cannot separate "the read failed" from "the account answered and its catalog was
// empty" — so an account that answered was told it "was never asked", in the one
// sentence whose whole job is to say what llz did and did not do.
func TestTheSeedSourceDoesNotClaimAnAccountWasNeverAsked(t *testing.T) {
	answered := seedSource(instanceresolve.K8sVersionChoice{Catalog: instanceresolve.CatalogAnswered})
	if strings.Contains(answered, "never asked") {
		t.Errorf("the account answered — an empty catalog is an answer; got %q", answered)
	}
	if !strings.Contains(answered, "no full build id") {
		t.Errorf("it must say what the answer was missing, not merely that a default was used; got %q", answered)
	}

	unasked := seedSource(instanceresolve.K8sVersionChoice{})
	if !strings.Contains(unasked, "never asked") {
		t.Errorf("with no successful read llz genuinely did not ask, and must say so rather than "+
			"implying it judged a catalog; got %q", unasked)
	}

	derived := seedSource(instanceresolve.K8sVersionChoice{Catalog: instanceresolve.CatalogAnswered, Newest: "v1.34.6+lke2"})
	if !strings.Contains(derived, "newest") {
		t.Errorf("a derived seed must name where it came from; got %q", derived)
	}
}

// misspellShared rewrites landingzone.yaml's shared pin to a spelling the account
// rejects but which names the same version, and returns the deployment dir.
func misspellShared(t *testing.T, dir, from, to string) {
	t.Helper()
	lzPath := filepath.Join(dir, clusterspec.LandingZoneFile)
	b, err := os.ReadFile(lzPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), from) {
		t.Fatalf("premise: %s does not carry %s", lzPath, from)
	}
	if err := os.WriteFile(lzPath, []byte(strings.Replace(string(b), from, to, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestASpellingSlipInSpecDefaultsIsNotReportedAsARotation.
//
// docs/runbooks/first-build-failed.md documents both misspellings — a missing
// leading `v`, a missing `+lke` suffix — and terraform sends the pin VERBATIM, so
// the account rejects them while building the version the operator meant.
// `ReplacementForInheritedPin` corrects the spelling; the MESSAGE still said "which
// this Linode account can no longer build", which describes a rotation and sends
// an operator hunting a replacement for a version sitting in their own catalog.
func TestASpellingSlipInSpecDefaultsIsNotReportedAsARotation(t *testing.T) {
	dir := t.TempDir()
	catalog := []string{"v1.34.6+lke2", "v1.32.9+lke4"}
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: catalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	misspellShared(t, dir, "v1.34.6+lke2", "1.34.6+lke2") // the leading `v`

	var runErr error
	out := captureStderr(t, func() {
		_, runErr = scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: catalog},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if runErr != nil {
		t.Fatalf("second `llz env add`: %v", runErr)
	}
	if !strings.Contains(out, "spelled differently") {
		t.Errorf("a one-character misspelling was not reported as one:\n%s", out)
	}
	if strings.Contains(out, "can no longer build") {
		t.Errorf("the account builds this version happily; calling it a rotation sends the\n"+
			"operator looking for a replacement:\n%s", out)
	}
	// The deployment gets the SAME version in the catalog's spelling, not an upgrade.
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := inst.Env("dr"); strings.TrimSpace(e.Cluster.K8sVersion) != "v1.34.6+lke2" {
		t.Errorf("dr pins %q, want the catalog's spelling of the version spec.defaults meant",
			e.Cluster.K8sVersion)
	}
}

// TestTheDivergenceRemedyOwnsItsCostAndNamesBothEdits.
//
// Two defects in one message. It promised the running deployments are untouched
// and then offered a remedy that moves them — a control-plane upgrade nobody
// scheduled, which is the whole reason the per-deployment override exists. And it
// named only the spec.defaults edit: leave the override behind and it is identical
// to the new shared value, therefore invisible, and this deployment silently stops
// tracking every later bump.
func TestTheDivergenceRemedyOwnsItsCostAndNamesBothEdits(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: theOtherAccountCatalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	rotated := []string{"v1.34.6+lke2", "v1.32.9+lke4"}
	var runErr error
	out := captureStderr(t, func() {
		_, runErr = scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: rotated},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if runErr != nil {
		t.Fatalf("second `llz env add`: %v", runErr)
	}
	if !strings.Contains(out, "including the running ones") {
		t.Errorf("the remedy reverses the \"running deployments are untouched\" guarantee printed\n"+
			"four lines above it without saying so:\n%s", out)
	}
	for _, want := range []string{"delete", "environments/dr.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("the remedy never says to %q, so following it leaves an invisible override\n"+
				"that freezes dr out of later shared bumps:\n%s", want, out)
		}
	}
}

// TestTheDivergencePreviewDoesNotDescribeWritesItDidNotMake.
//
// printK8sVersionConsequences is called from BOTH paths — deliberately, so
// `--dry-run` previews the version decision — which makes it easy for its wording
// to describe writes that have not happened. It said "Pinning v1.34.6+lke2 for
// "dr" alone so it can be created", and told the operator to delete a line from a
// file the run never created.
func TestTheDivergencePreviewDoesNotDescribeWritesItDidNotMake(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: theOtherAccountCatalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	rotated := []string{"v1.34.6+lke2"}
	var runErr error
	out := captureStderr(t, func() {
		_, runErr = scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: rotated},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1", DryRun: true})
	})
	if runErr != nil {
		t.Fatalf("dry-run `llz env add`: %v", runErr)
	}
	if _, err := os.Stat(filepath.Join(dir, clusterspec.EnvironmentsDir, "dr.yaml")); err == nil {
		t.Fatal("--dry-run authored environments/dr.yaml")
	}
	if strings.Contains(out, "Pinning ") {
		t.Errorf("a --dry-run preview describes the pin as already made:\n%s", out)
	}
	if !strings.Contains(out, "Would pin ") {
		t.Errorf("the --dry-run preview does not say the pin is hypothetical:\n%s", out)
	}
	if strings.Contains(out, "the line this run adds to") {
		t.Errorf("a --dry-run preview tells the operator to edit a file it never created:\n%s", out)
	}
}

// TestADeploymentIsNotAuthoredAgainstNoVersionWhenTheAccountAnswered.
//
// A landingzone.yaml that PARSES and names no spec.defaults.cluster.k8sVersion —
// hand-authored from the example, or edited down — leaves a new deployment with no
// version at all. `llz env add` wrote the env file anyway and walked into `llz
// render`'s "cluster.k8sVersion is required", landing in the
// env-file-without-overlay dead end this command has its own recovery error for —
// while holding the account's answer and discarding it. That is the failure this
// feature exists to remove, wearing a different hat.
//
// Per-deployment and not seeded into spec.defaults: EnsureLandingZone only writes
// a file it CREATES, and silently editing an existing landingzone.yaml is a much
// bigger licence than `llz env add` has ever taken. The message says how to make
// it shared.
func TestADeploymentIsNotAuthoredAgainstNoVersionWhenTheAccountAnswered(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: theOtherAccountCatalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	// Remove the first deployment: an EXISTING one inherits the shared pin, so
	// stripping it would make the instance invalid before this command runs and the
	// failure would be `llz validate`'s, not the one under test. The reachable state
	// is a hand-authored spec with no shared pin and no deployments yet.
	if err := os.RemoveAll(filepath.Join(dir, clusterspec.EnvironmentsDir, "lab.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "apl-values", "lab")); err != nil {
		t.Fatal(err)
	}
	lzPath := filepath.Join(dir, clusterspec.LandingZoneFile)
	b, err := os.ReadFile(lzPath)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, "k8sVersion:") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(lzPath, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if lz, err := clusterspec.Load(lzPath); err != nil {
		t.Fatalf("premise: the stripped spec must still parse: %v", err)
	} else if strings.TrimSpace(lz.Spec.Defaults.Cluster.K8sVersion) != "" {
		t.Fatal("premise: the shared pin was not actually removed")
	}

	var runErr error
	out := captureStderr(t, func() {
		_, runErr = scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: []string{"v1.34.6+lke2"}},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if runErr != nil {
		t.Fatalf("second `llz env add`: %v", runErr)
	}
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := inst.Env("dr")
	if !ok {
		t.Fatal("the deployment was not authored")
	}
	if got := strings.TrimSpace(e.Cluster.K8sVersion); got != "v1.34.6+lke2" {
		t.Errorf("the deployment carries k8sVersion %q — `llz render` rejects it with\n"+
			"\"cluster.k8sVersion is required\", and the account's answer was in hand all along", got)
	}
	for _, want := range []string{"nothing to inherit", "llz spec set defaults.cluster.k8sVersion"} {
		if !strings.Contains(out, want) {
			t.Errorf("a per-deployment pin was written and nothing said %q:\n%s", want, out)
		}
	}
}

// TestSeedingAnUntestedMinorIsAnnounced.
//
// `llz doctor` and `llz ci assert-k8s-version` ask only whether the ACCOUNT can
// build the pin — a different question from whether this llz release and the
// apl-core baseline have been seen working on that minor. So the account offering
// a brand-new minor the week Linode publishes it is enough for every gate to pass,
// and a fresh instance lands there with nothing said.
//
// This does not revisit the newest-offered choice, only make it visible: the value
// is written once, at scaffold time, and #455 adopts a running cluster's version
// rather than re-seeding over it. What it must not do is move silently.
func TestSeedingAnUntestedMinorIsAnnounced(t *testing.T) {
	tested := envdef.SeedK8sVersion("")
	if tested == "" {
		t.Fatal("premise: there must be a compiled fallback to compare against")
	}
	// A catalog on a far-future minor, which also still offers the tested one.
	newer := "v9.99.1+lke3"
	var runErr error
	out := captureStderr(t, func() {
		_, runErr = scaffoldWith(t, &fakeCatalog{versions: []string{newer, tested}},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if runErr != nil {
		t.Fatalf("llz env add: %v", runErr)
	}
	for _, want := range []string{newer, tested, "MINOR", "llz spec set defaults.cluster.k8sVersion=" + tested} {
		if !strings.Contains(out, want) {
			t.Errorf("a fresh instance was seeded onto an untested minor and nothing said %q:\n%s",
				want, out)
		}
	}
	// NOT `--k8s-version`, which pins ONE deployment and never becomes
	// spec.defaults, so re-running with it would leave the shared default on the
	// newer minor and merely SPLIT the instance — the opposite of the intent.
	if strings.Contains(out, "--k8s-version "+tested) {
		t.Errorf("the remedy splits the instance instead of moving the shared default:\n%s", out)
	}

	// Staying on the tested minor says nothing at all.
	quiet := captureStderr(t, func() {
		if _, err := scaffoldWith(t, &fakeCatalog{versions: []string{tested}},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
			t.Fatalf("llz env add: %v", err)
		}
	})
	if strings.Contains(quiet, "MINOR") {
		t.Errorf("a seed on the tested minor warned about minors:\n%s", quiet)
	}
}

// TestTheOfflineOperatorStillGetsARenderableSpecWithNoSharedPin.
//
// k8s.Newest is empty precisely when the account could NOT be asked — the offline
// or expired-token operator — so keying the no-shared-pin fix on it left exactly
// that person with no pin at all: the env file authored without a version, `llz
// render` rejecting it, and the dead end reached in silence. The compiled literal
// may be stale, but a spec that renders beats one that cannot, and `llz doctor`
// re-checks it before anything is built.
func TestTheOfflineOperatorStillGetsARenderableSpecWithNoSharedPin(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: theOtherAccountCatalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, clusterspec.EnvironmentsDir, "lab.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "apl-values", "lab")); err != nil {
		t.Fatal(err)
	}
	lzPath := filepath.Join(dir, clusterspec.LandingZoneFile)
	b, err := os.ReadFile(lzPath)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, "k8sVersion:") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(lzPath, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	// nil catalog = no token at all, so k8s.Newest is "".
	var runErr error
	out := captureStderr(t, func() {
		_, runErr = scaffoldEnv(t, dir, "dr", nil,
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if runErr != nil {
		t.Fatalf("offline `llz env add`: %v", runErr)
	}
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := inst.Env("dr")
	if !ok {
		t.Fatal("the deployment was not authored")
	}
	if got := strings.TrimSpace(e.Cluster.K8sVersion); got != envdef.SeedK8sVersion("") {
		t.Errorf("offline, with no shared pin, the deployment carries k8sVersion %q — `llz render`\n"+
			"rejects that with \"cluster.k8sVersion is required\" and nothing said so", got)
	}
	if !strings.Contains(out, "nothing to inherit") {
		t.Errorf("the offline operator got no warning about the pin llz chose for them:\n%s", out)
	}
	// AND IT MUST NOT CLAIM THE ACCOUNT CHOSE IT. The message said "the newest your
	// account offers" unconditionally; offline that is llz's compiled fallback, and
	// the account was never reached at all — a claim about the account on the one
	// path where nobody asked it.
	if strings.Contains(out, "the newest your account offers") {
		t.Errorf("offline, the pin came from llz's compiled fallback and the run credited it to\n"+
			"the account:\n%s", out)
	}
}

// TestTheUntestedMinorRemedyIsNotOfferedAgainstASpecTheDryRunNeverWrote — this was
// the one consequence message that ignored dryRun, so `--dry-run` offered an
// `llz spec set` against a landingzone.yaml the run never created.
func TestTheUntestedMinorRemedyIsNotOfferedAgainstASpecTheDryRunNeverWrote(t *testing.T) {
	tested := envdef.SeedK8sVersion("")
	out := captureStderr(t, func() {
		if _, err := scaffoldWith(t, &fakeCatalog{versions: []string{"v9.99.1+lke3", tested}},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1", DryRun: true}); err != nil {
			t.Fatalf("dry-run `llz env add`: %v", err)
		}
	})
	if !strings.Contains(out, "MINOR") {
		t.Fatalf("premise: the dry run should still preview the minor warning:\n%s", out)
	}
	if !strings.Contains(out, "after a real run") {
		t.Errorf("a --dry-run preview offers `llz spec set` against a spec it never wrote:\n%s", out)
	}
	// AND THE CLAUSE THAT NAMES THE WRITE MUST AGREE WITH IT. chosenField asserts a
	// write in the same sentence as the "after a real run (this one wrote nothing)"
	// clause — "spec.defaults.cluster.k8sVersion is seeded with X" about a file this
	// run never created, one line above an admission that it wrote nothing.
	if strings.Contains(out, "k8sVersion is seeded with") {
		t.Errorf("a --dry-run preview states the seed as done, in the same message that says it\n"+
			"wrote nothing:\n%s", out)
	}
	if !strings.Contains(out, "would be seeded with") {
		t.Errorf("the --dry-run preview does not put the seed in the conditional:\n%s", out)
	}
}

// stripSharedPin removes spec.defaults.cluster.k8sVersion from landingzone.yaml,
// leaving a spec that still parses. Deployments keep their own pins.
func stripSharedPin(t *testing.T, dir string) {
	t.Helper()
	lzPath := filepath.Join(dir, clusterspec.LandingZoneFile)
	b, err := os.ReadFile(lzPath)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, "k8sVersion:") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(lzPath, []byte(strings.Join(kept, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWithNoSharedPinTheNewDeploymentJoinsItsSiblingsMinor.
//
// ReplacementForInheritedPin prefers the family's minor when a SHARED pin rotates
// out. A shared pin that was never there is not a reason to abandon that rule: with
// no spec.defaults every existing deployment carries its own version — the spec
// does not validate otherwise — so when they agree on a minor, that is this
// instance's family and the new deployment belongs in it. Taking the account's
// absolute newest instead is the same silent skew, reached down a different path.
func TestWithNoSharedPinTheNewDeploymentJoinsItsSiblingsMinor(t *testing.T) {
	dir := t.TempDir()
	family := []string{"v9.98.4+lke1"}
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: family},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	// Give lab its own pin, then remove the shared default: the hand-authored shape
	// where each deployment names its own version.
	labFile := filepath.Join(dir, clusterspec.EnvironmentsDir, "lab.yaml")
	b, err := os.ReadFile(labFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(labFile, append(b, []byte("\n    k8sVersion: v9.98.4+lke1\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	stripSharedPin(t, dir)

	// The account now offers a much newer minor alongside the family's.
	catalog := []string{"v9.99.9+lke9", "v9.98.7+lke3"}
	var runErr error
	out := captureStderr(t, func() {
		_, runErr = scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: catalog},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if runErr != nil {
		t.Fatalf("second `llz env add`: %v", runErr)
	}
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := inst.Env("dr")
	if got := strings.TrimSpace(e.Cluster.K8sVersion); got != "v9.98.7+lke3" {
		t.Errorf("dr pins %q, want v9.98.7+lke3 — the newest build of the minor its sibling runs", got)
	}
	if !strings.Contains(out, "the minor your other deployments run") {
		t.Errorf("the run joined the family's minor and did not say so:\n%s", out)
	}
	// AND IT MUST NOT THEN SECOND-GUESS ITSELF. The family's minor is not the minor
	// this llz release tests, and warning about that would argue with the choice the
	// same run just made to keep dr beside lab.
	if strings.Contains(out, "MINOR from") {
		t.Errorf("the run kept dr with its siblings and then warned that it should not have:\n%s", out)
	}
}

// TestAPerDeploymentPinOnANewMinorIsAnnouncedToo — the untested-minor warning was
// gated on !lzExists, so the no-shared-pin path could pin a brand-new minor
// per-deployment in silence: the exact thing the warning exists to prevent,
// reached down the path added after it.
func TestAPerDeploymentPinOnANewMinorIsAnnouncedToo(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: theOtherAccountCatalog},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	// No siblings left to join, and no shared pin.
	if err := os.RemoveAll(filepath.Join(dir, clusterspec.EnvironmentsDir, "lab.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "apl-values", "lab")); err != nil {
		t.Fatal(err)
	}
	stripSharedPin(t, dir)

	// The tested minor IS on offer, so the remedy branch fires and can be checked;
	// the newest is on a far-future minor, so that is what llz picks.
	var runErr error
	out := captureStderr(t, func() {
		_, runErr = scaffoldEnv(t, dir, "dr",
			&fakeCatalog{versions: []string{"v9.99.1+lke3", envdef.SeedK8sVersion("")}},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if runErr != nil {
		t.Fatalf("second `llz env add`: %v", runErr)
	}
	if !strings.Contains(out, "this deployment is pinned to") {
		t.Errorf("a per-deployment pin on an untested minor was chosen in silence:\n%s", out)
	}
	// AND THE REMEDY MUST NAME THE FILE THAT GOVERNS. This run writes a
	// per-deployment cluster.k8sVersion, which SHADOWS spec.defaults — so
	// `llz spec set defaults…` would leave the deployment exactly where it is, while
	// the warning beside it tells the operator to set defaults to a different
	// version. Two remedies, one file, opposite values.
	// THE TWO MESSAGES MUST NOT NAME OPPOSITE VERSIONS FOR THE SAME FIELD. The
	// no-shared-pin block offers to promote llz's choice to the instance default;
	// when that choice is the untested one the warning above just asked the operator
	// to move off, promoting it is the opposite instruction — and it also deletes the
	// override that would have held the line.
	if strings.Contains(out, "llz spec set defaults.cluster.k8sVersion=v9.99.1+lke3") {
		t.Errorf("one message says to move off v9.99.1+lke3 and the next offers to make it the\n"+
			"instance default:\n%s", out)
	}
	if !strings.Contains(out, "llz spec set defaults.cluster.k8sVersion="+envdef.SeedK8sVersion("")) {
		t.Errorf("the shared-default remedy does not name the tested version the warning above it\n"+
			"asked for:\n%s", out)
	}
	if !strings.Contains(out, "llz env set dr cluster.k8sVersion=") {
		t.Errorf("the remedy does not name environments/dr.yaml, the file this run actually\n"+
			"wrote the pin into:\n%s", out)
	}
}

// TestAReplacementThatAbandonsTheFamilyIsAnnounced.
//
// ReplacementForInheritedPin keeps a deployment in its own minor when it can and
// falls through to the account's newest only when that minor is GONE — an
// unconstrained choice, and the last one the untested-minor warning did not cover.
// A spelling fix and a same-minor replacement both stay put, and warning about
// those would argue with the rule that produced them, which is why the arm keys on
// DifferentMinor against the pin it replaced.
func TestAReplacementThatAbandonsTheFamilyIsAnnounced(t *testing.T) {
	dir := t.TempDir()
	// The instance is seeded on a family that will vanish entirely.
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: []string{"v9.97.1+lke1"}},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	// Neither v9.97 nor the tested minor is on offer any more.
	var runErr error
	out := captureStderr(t, func() {
		_, runErr = scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: []string{"v9.99.1+lke3"}},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if runErr != nil {
		t.Fatalf("second `llz env add`: %v", runErr)
	}
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := inst.Env("dr"); strings.TrimSpace(e.Cluster.K8sVersion) != "v9.99.1+lke3" {
		t.Fatalf("premise: the replacement should have abandoned the family, got %q",
			e.Cluster.K8sVersion)
	}
	if !strings.Contains(out, "this deployment is pinned to") {
		t.Errorf("the replacement left the family for an untested minor in silence:\n%s", out)
	}
}

// TestASameMinorReplacementIsNotSecondGuessed — the negative arm. When the
// deployment stays in its own family, the untested-minor warning must not fire:
// that choice was made deliberately to keep it beside its siblings.
func TestASameMinorReplacementIsNotSecondGuessed(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: []string{"v9.98.4+lke1"}},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	// That build rotates out; its MINOR is still offered, alongside a newer one.
	var runErr error
	out := captureStderr(t, func() {
		_, runErr = scaffoldEnv(t, dir, "dr",
			&fakeCatalog{versions: []string{"v9.99.9+lke9", "v9.98.7+lke3"}},
			envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"})
	})
	if runErr != nil {
		t.Fatalf("second `llz env add`: %v", runErr)
	}
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := inst.Env("dr"); strings.TrimSpace(e.Cluster.K8sVersion) != "v9.98.7+lke3" {
		t.Fatalf("premise: the replacement should stay in the family, got %q", e.Cluster.K8sVersion)
	}
	if strings.Contains(out, "MINOR from") {
		t.Errorf("the run kept dr in its own minor and then warned that it should not have:\n%s", out)
	}
}

// TestTheSiblingScanIsKeyedTheWayEnvIsKeyed.
//
// existingDeployments returns FILE BASENAMES; Env() is keyed on metadata.name.
// Looking one up with the other works only while the two happen to agree — and the
// no-shared-pin path exists precisely for hand-authored specs, which are exactly
// where they diverge. Keyed wrong, the instance silently loses its family and the
// new deployment takes the account's absolute newest instead.
func TestTheSiblingScanIsKeyedTheWayEnvIsKeyed(t *testing.T) {
	dir := t.TempDir()
	family := []string{"v9.98.4+lke1"}
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: family},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	labFile := filepath.Join(dir, clusterspec.EnvironmentsDir, "lab.yaml")
	b, err := os.ReadFile(labFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(labFile, append(b, []byte("\n    k8sVersion: v9.98.4+lke1\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	// THE DIVERGENCE, and it is legal: the file is named one thing, metadata.name
	// says another, and Env() answers to the latter.
	renamed := filepath.Join(dir, clusterspec.EnvironmentsDir, "zz-renamed.yaml")
	if err := os.Rename(labFile, renamed); err != nil {
		t.Fatal(err)
	}
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := inst.Env("lab"); !ok {
		t.Skip("this spec model does not key envs on metadata.name; the finding does not apply")
	}
	stripSharedPin(t, dir)

	if _, err := scaffoldEnv(t, dir, "dr",
		&fakeCatalog{versions: []string{"v9.99.9+lke9", "v9.98.7+lke3"}},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("second `llz env add`: %v", err)
	}
	got, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := got.Env("dr")
	if v := strings.TrimSpace(e.Cluster.K8sVersion); v != "v9.98.7+lke3" {
		t.Errorf("dr pins %q, want v9.98.7+lke3 — the sibling family was found by file basename\n"+
			"rather than by the key Env() answers to, so a renamed spec lost its family", v)
	}
}

// TestOfflineTheNewDeploymentStillJoinsItsSiblingsFamily.
//
// The sibling-minor rule went through the account CATALOG, so it was silently
// inert with no token: k8s.Offered is nil, the lookup finds nothing, and the pin
// falls through to llz's compiled literal — possibly two minors from the family
// sitting right there in environments/. Nothing named the skew either, because the
// untested-minor warning compares against that same literal and so saw no
// difference. The family is readable from disk; the account is not the only source.
func TestOfflineTheNewDeploymentStillJoinsItsSiblingsFamily(t *testing.T) {
	dir := t.TempDir()
	family := "v9.98.4+lke1"
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: []string{family}},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	labFile := filepath.Join(dir, clusterspec.EnvironmentsDir, "lab.yaml")
	b, err := os.ReadFile(labFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(labFile, append(b, []byte("\n    k8sVersion: "+family+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	stripSharedPin(t, dir)

	// nil catalog = no token at all.
	if _, err := scaffoldEnv(t, dir, "dr", nil,
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("offline `llz env add`: %v", err)
	}
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := inst.Env("dr")
	got := strings.TrimSpace(e.Cluster.K8sVersion)
	if got == envdef.SeedK8sVersion("") {
		t.Errorf("offline, dr took llz's compiled literal %q while its sibling runs %s — the family\n"+
			"was on disk the whole time and nothing named the skew", got, family)
	}
	if got != family {
		t.Errorf("dr pins %q, want %s — the version this instance demonstrably runs", got, family)
	}
}

// TestACatalogThatAnsweredAndLacksTheFamilyStillMoves — the negative arm. A
// catalog that WAS read and holds no build of the siblings' minor means the minor
// is genuinely gone, and pinning a version the account has stopped offering would
// be worse than moving.
func TestACatalogThatAnsweredAndLacksTheFamilyStillMoves(t *testing.T) {
	dir := t.TempDir()
	family := "v9.98.4+lke1"
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: []string{family}},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	labFile := filepath.Join(dir, clusterspec.EnvironmentsDir, "lab.yaml")
	b, err := os.ReadFile(labFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(labFile, append(b, []byte("\n    k8sVersion: "+family+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	stripSharedPin(t, dir)

	// The account answered, and that family is gone from it.
	if _, err := scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: []string{"v9.99.9+lke9"}},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("second `llz env add`: %v", err)
	}
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := inst.Env("dr"); strings.TrimSpace(e.Cluster.K8sVersion) != "v9.99.9+lke9" {
		t.Errorf("dr pins %q — the account was asked and no longer offers %s, so pinning it would\n"+
			"write a version that cannot be built", e.Cluster.K8sVersion, family)
	}
}

// pinSibling gives an existing deployment its own cluster.k8sVersion.
func pinSibling(t *testing.T, dir, env, version string) {
	t.Helper()
	f := filepath.Join(dir, clusterspec.EnvironmentsDir, env+".yaml")
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, append(b, []byte("\n    k8sVersion: "+version+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAMisspelledSiblingPinIsNeverCopiedIntoANewDeployment.
//
// sharedSiblingMinor's answer can BECOME the pin llz writes, and terraform sends
// it verbatim — so a sibling carrying one of the two misspellings the runbook
// documents (`v1.33.6`, no `+lke`) would have been copied into the new deployment
// and killed its first apply on `[400] k8s_version is not valid`. clusterspec only
// checks the field is non-empty, so nothing downstream catches it either. Every
// other choosing path here is fenced by a build-id check; this one was not.
func TestAMisspelledSiblingPinIsNeverCopiedIntoANewDeployment(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: []string{"v9.98.4+lke1"}},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	pinSibling(t, dir, "lab", "v9.98.4") // the `+lke` suffix left off
	stripSharedPin(t, dir)

	if _, err := scaffoldEnv(t, dir, "dr", nil, // offline, so the sibling arm is live
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("offline `llz env add`: %v", err)
	}
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := inst.Env("dr")
	got := strings.TrimSpace(e.Cluster.K8sVersion)
	if got == "v9.98.4" {
		t.Errorf("dr copied its sibling's misspelled pin verbatim; terraform sends that as-is and\n" +
			"the apply dies on [400] k8s_version is not valid")
	}
	if !linode.NamesABuild(got) {
		t.Errorf("dr pins %q, which is not a full LKE-E build id", got)
	}
}

// TestAnUnparseableSiblingDoesNotReadAsAgreement — DifferentMinor answers false
// when either side names no minor, so a sibling pinned `latest` read as AGREEMENT
// with everything, and if it sorted first it became the family value itself.
func TestAnUnparseableSiblingDoesNotReadAsAgreement(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldEnv(t, dir, "aaa", &fakeCatalog{versions: []string{"v9.98.4+lke1"}},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("first `llz env add`: %v", err)
	}
	if _, err := scaffoldEnv(t, dir, "bbb", &fakeCatalog{versions: []string{"v9.98.4+lke1"}},
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("second `llz env add`: %v", err)
	}
	// `latest+lke1` sorts first and PASSES the build-id fence — it carries a `+lke`
	// suffix — while naming no minor at all. So the build-id check alone is not
	// enough: DifferentMinor answers false against it, it reads as agreement with
	// everything, and it becomes the family value that gets written.
	pinSibling(t, dir, "aaa", "latest+lke1")
	pinSibling(t, dir, "bbb", "v9.98.4+lke1")
	stripSharedPin(t, dir)

	if _, err := scaffoldEnv(t, dir, "dr", nil,
		envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
		t.Fatalf("offline `llz env add`: %v", err)
	}
	inst, err := clusterspec.LoadInstance(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := inst.Env("dr")
	if got := strings.TrimSpace(e.Cluster.K8sVersion); got == "latest+lke1" {
		t.Errorf("a sibling pin that names no minor became the family value and was written into dr")
	} else if got != "v9.98.4+lke1" {
		t.Errorf("dr pins %q, want the one sibling that names a real build id", got)
	}
}

// TestACatalogThatAnsweredWithNothingUsableStillJoinsTheFamily.
//
// The arm was keyed on "the catalog was never read" (CatalogNotAsked) — but
// accountLKEVersions reports a SUCCESSFUL read for an empty listing and for an
// all-coarse catalog. In both, Offered holds no parseable build and Newest is "",
// so the sibling arm stayed inert in two more states than the offline one, and the
// untested-minor warning was silent too because `chosen` was empty. "The account
// gave me nothing I can write" is `k8s.Newest == ""`.
func TestACatalogThatAnsweredWithNothingUsableStillJoinsTheFamily(t *testing.T) {
	for _, tc := range []struct {
		name    string
		catalog []string
	}{
		{"a successful but empty listing", []string{}},
		{"a catalog naming no build ids at all", []string{"9.98", "9.99"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := scaffoldEnv(t, dir, "lab", &fakeCatalog{versions: []string{"v9.98.4+lke1"}},
				envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
				t.Fatalf("first `llz env add`: %v", err)
			}
			pinSibling(t, dir, "lab", "v9.98.4+lke1")
			stripSharedPin(t, dir)

			if _, err := scaffoldEnv(t, dir, "dr", &fakeCatalog{versions: tc.catalog},
				envdef.Opts{Region: "us-ord", ObjCluster: "us-ord-1"}); err != nil {
				t.Fatalf("second `llz env add`: %v", err)
			}
			inst, err := clusterspec.LoadInstance(dir)
			if err != nil {
				t.Fatal(err)
			}
			e, _ := inst.Env("dr")
			if got := strings.TrimSpace(e.Cluster.K8sVersion); got != "v9.98.4+lke1" {
				t.Errorf("dr pins %q — the catalog answered with nothing llz can write, and the\n"+
					"family was on disk the whole time", got)
			}
		})
	}
}
