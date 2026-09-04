package overlay

// otomiversion_test.go — the platform version must actually REACH the cluster.
//
// The render half of this feature shipped first: `llz render` wrote
// apl-values/<env>/apl-overlay/otomi.yaml whenever
// spec.cluster.bootstrap.manageAplVersion was set. Nothing consumed it.
// aplOverlayTargets maps only obj.yaml, and apps and teams have their own target
// functions — so the file was rendered, committed and reviewed, and never reached
// the apl-<env> branch or apl-core. `manageAplVersion: true` looked wired and did
// nothing.
//
// Found on a live managed cluster, by going to look for where the version actually
// lives. These tests are the consumer-side gate that would have caught it: they
// assert on what lands on the TARGET branch, not on what render produced.

import (
	"context"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/metrics"
)

// branchRepo is fakeRepo with the branch actually honoured. The shared fake keys
// one map by path alone, which cannot express "the source branch says v6.2.1-rc.2
// while the target branch still says v6.2.1" — and that difference is the entire
// behaviour under test.
type branchRepo struct {
	src, tgt  map[string]string
	gotFiles  map[string]string
	gotBranch string
}

func (r *branchRepo) ReadFile(_ context.Context, branch, path string) (string, bool, error) {
	m := r.tgt
	if branch == "main" {
		m = r.src
	}
	c, ok := m[path]
	return c, ok && c != "", nil
}

func (r *branchRepo) OverlayCommit(_ context.Context, branch string, files map[string]string, _ string, _ int) (string, bool, error) {
	r.gotFiles, r.gotBranch = files, branch
	return "sha", true, nil
}

// The live CR, verbatim from a managed e2e cluster's apl-<env> branch — apl-core
// writes it ("updated values [ci skip]") and owns every key but `version`.
const liveOtomiTarget = `kind: AplCapabilitySet
metadata:
    name: otomi
spec:
    aiEnabled: false
    isMultitenant: true
    isPreInstalled: true
    useORCS: true
    version: v6.2.1
`

func otomiRepo(sourceVersion string) *branchRepo {
	src := map[string]string{}
	if sourceVersion != "" {
		src[envOverlayPath("primary", clusterspec.OverlayOtomiFile)] =
			clusterspec.RenderOtomiOverlayEnv(clusterspec.Bootstrap{
				ManageAplVersion: true, AplChartVersion: sourceVersion,
			})
	}
	return &branchRepo{src: src, tgt: map[string]string{aplOtomiTarget: liveOtomiTarget}}
}

// THE GATE. An opted-in instance's version must land on the machine branch — the
// assertion is on the TARGET file, which is the only thing apl-core ever reads.
func TestOptedInVersionReachesTheMachineBranch(t *testing.T) {
	repo := otomiRepo("6.2.1-rc.2")
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got, ok := repo.gotFiles[aplOtomiTarget]
	if !ok {
		t.Fatalf("%s was never written — the rendered overlay reached nothing: %v", aplOtomiTarget, keysOf(repo.gotFiles))
	}
	if !strings.Contains(got, "v6.2.1-rc.2") {
		t.Errorf("the asserted version did not land:\n%s", got)
	}
	// apl-core's own settings survive: LLZ owns one key of this file.
	for _, keep := range []string{"isMultitenant: true", "useORCS: true", "isPreInstalled: true", "aiEnabled: false"} {
		if !strings.Contains(got, keep) {
			t.Errorf("the merge dropped apl-core's %q:\n%s", keep, got)
		}
	}
}

// NOT OPTED IN MEANS SILENCE, not an empty write. Linode owns the version on
// managed by default, and blanking spec.version would be the obj-overlay `{}`
// regression in a new file.
func TestWithoutTheOptInNothingIsWritten(t *testing.T) {
	repo := otomiRepo("")
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := repo.gotFiles[aplOtomiTarget]; ok {
		t.Errorf("no opt-in must mean no opinion, but %s was written: %q", aplOtomiTarget, repo.gotFiles[aplOtomiTarget])
	}
}

// AN ALREADY-CORRECT VERSION MUST NOT CHURN. apl-core rewrites this file on its own
// schedule; a reconciler that pushed every pass would fight it forever.
func TestAnAlreadyCorrectVersionDoesNotPush(t *testing.T) {
	repo := otomiRepo("6.2.1") // the target branch already says v6.2.1
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := repo.gotFiles[aplOtomiTarget]; ok {
		t.Errorf("the version already matches — pushing it again churns against apl-core: %q", repo.gotFiles[aplOtomiTarget])
	}
}

// A TARGET THAT DOES NOT EXIST YET IS NOT A REASON TO INVENT ONE. apl-operator
// seeds env/settings/otomi.yaml; writing a version-only file over its absence would
// hand apl-core a CR missing every other setting.
func TestAnAbsentTargetIsNotCreated(t *testing.T) {
	repo := otomiRepo("6.2.1-rc.2")
	repo.tgt = map[string]string{} // apl-operator has not seeded it
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := repo.gotFiles[aplOtomiTarget]; ok {
		t.Errorf("a version-only CR must not be created over an absent target: %q", repo.gotFiles[aplOtomiTarget])
	}
}
