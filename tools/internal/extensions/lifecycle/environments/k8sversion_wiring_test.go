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
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scaffolding over a live cluster at a rotated-out version must say %q; stderr was:\n%s", want, out)
		}
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
		choice       instanceresolve.K8sVersionChoice
		lzExists     bool
		inheritedFix string
		want         string
	}{
		"explicit pin wins": {
			instanceresolve.K8sVersionChoice{Pin: "v1.32.9+lke4", Newest: "v1.34.6+lke2", Offered: catalog},
			false, "", "v1.32.9+lke4",
		},
		"first env add shows the derived version": {
			instanceresolve.K8sVersionChoice{Newest: "v1.34.6+lke2", Offered: catalog}, false, "", "v1.34.6+lke2",
		},
		"later env add inherits": {
			instanceresolve.K8sVersionChoice{Newest: "v1.34.6+lke2", Offered: catalog}, true, "", "inherited",
		},
		"later env add overrides a rotated-out shared pin": {
			instanceresolve.K8sVersionChoice{Newest: "v1.34.6+lke2", Offered: catalog}, true, "v1.34.6+lke2", "this deployment only",
		},
		// THE TWO STATES THAT USED TO RENDER IDENTICALLY. One never reached the
		// account; the other got an answer it cannot use — and the second one prints
		// the catalog to stderr moments earlier, so "could not be asked" contradicted
		// a message already on screen.
		"account unreachable": {
			instanceresolve.K8sVersionChoice{}, false, "", "could not be asked",
		},
		"catalog answered but names no build id": {
			instanceresolve.K8sVersionChoice{Offered: []string{"1.33", "1.34"}}, false, "", "names no build id",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := k8sVersionBanner(c.choice, c.lzExists, c.inheritedFix)
			if !strings.Contains(got, c.want) {
				t.Errorf("banner = %q, want it to contain %q", got, c.want)
			}
		})
	}
	// And the two "no build id" states must not render the same string, which is the
	// whole finding rather than a wording preference.
	unreachable := k8sVersionBanner(instanceresolve.K8sVersionChoice{}, false, "")
	coarse := k8sVersionBanner(instanceresolve.K8sVersionChoice{Offered: []string{"1.33"}}, false, "")
	if unreachable == coarse {
		t.Errorf("an unreachable account and a catalog llz cannot use both render %q — "+
			"they are different events with different remedies", unreachable)
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

// TestTheE2ELanePinsTheVersionWhenItReusesACluster.
//
// `k8s_version` reaches the LKE-E API on a create OR A CHANGE. release-e2e-warm
// refreshes a LIVE cluster on purpose (it exists to skip the ~14m create), and
// release-e2e keeps one on keep_cluster=true — so a re-scaffold that derives a
// different version than the running cluster plans a CONTROL-PLANE UPGRADE inside
// a run whose whole point was not to rebuild anything. `assert-k8s-version` cannot
// catch it: the freshly derived pin is in the catalog, it is simply not the one
// that cluster runs.
//
// The hazard predates the derivation — a baked literal mismatched a cluster
// created at anything else in exactly the same way — so the escape is an explicit
// pin, and it has to be reachable without editing the workflow.
func TestTheE2ELanePinsTheVersionWhenItReusesACluster(t *testing.T) {
	step := e2eScaffoldStep(t)
	if !strings.Contains(step, `--k8s-version "${E2E_K8S_VERSION}"`) {
		t.Errorf("the e2e scaffold cannot be pinned, so a lane that REUSES a cluster (release-e2e-warm, or\n"+
			"release-e2e with keep_cluster=true) re-derives the version and plans an unrequested\n"+
			"LKE-Enterprise control-plane upgrade on it. Step body:\n%s", step)
	}
	raw, err := os.ReadFile(e2eInstantiateWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	// From a repo VAR, so a maintainer keeping a warm cluster sets it without a
	// commit — and it must stay optional, since an empty value is what makes the
	// cold lane derive.
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
