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

// branchRepo is fakeRepo with the branch actually honoured, and with found meaning
// PRESENT rather than non-empty — which is what the shipped adapter does.
// ghgitdata.ReadFile returns found=true for a file that exists and is EMPTY; only a
// 404 is found=false. The package's other fake answers found=(content != ""), so it
// cannot express "exists and is empty", and that hid a defect which blanked
// apl-core's CR through exactly that state. A harness that cannot represent a real
// state is where the blind spot lives.
// The shared fake keys
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
	return c, ok, nil
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

// A DEGENERATE TARGET IS NOT A MERGE BASE.
//
// ghgitdata.ReadFile answers found=true for a file that exists and is EMPTY, so an
// empty / "{}" / "null" / comment-only settings CR arrives as "present". Merging
// into it produced a two-line spec.version document with no kind, no metadata and
// none of apl-core's eight settings, pushed over its CR — the {}-over-obj.yaml
// regression in a new file. Every one of these must write NOTHING.
func TestADegenerateTargetIsNeverMergedInto(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"empty", ""},
		{"newline only", "\n"},
		{"empty map", "{}\n"},
		{"null", "null\n"},
		{"comment only", "# apl-core placeholder\n"},
		{"no kind", "spec:\n  version: v6.2.1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := otomiRepo("6.2.2")
			repo.tgt = map[string]string{aplOtomiTarget: tc.target}
			if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got, ok := repo.gotFiles[aplOtomiTarget]; ok {
				t.Errorf("wrote over a degenerate CR: %q", got)
			}
		})
	}
}

// A VERSION apl-core WOULD REJECT MUST NOT REACH THE BRANCH. Its schema pattern is
// the authority, and a rejected value converges silently — the merge is a no-op on
// the next pass, so it sits there permanently refused with nothing red.
func TestAVersionAplCoreWouldRejectIsNotWritten(t *testing.T) {
	repo := otomiRepo("")
	repo.src[envOverlayPath("primary", clusterspec.OverlayOtomiFile)] =
		"kind: AplCapabilitySet\nspec:\n  version: '6.2.9'\n" // bare, no leading v
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got, ok := repo.gotFiles[aplOtomiTarget]; ok {
		t.Errorf("apl-core's schema rejects that version — it must not be written: %q", got)
	}
}

// A MULTI-DOCUMENT TARGET MUST NOT BE TRUNCATED. The merge re-marshals one
// document, so writing back would silently drop the rest of a file LLZ does not own.
func TestAMultiDocumentTargetIsNotTruncated(t *testing.T) {
	repo := otomiRepo("6.2.2")
	repo.tgt = map[string]string{aplOtomiTarget: liveOtomiTarget + "---\nkind: Other\nspec:\n  keep: true\n"}
	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got, ok := repo.gotFiles[aplOtomiTarget]; ok {
		t.Errorf("a multi-document target must be left alone, got: %q", got)
	}
}

// A MALFORMED TARGET MUST NOT WEDGE THE REST OF THE PASS. This runs last, after
// obj.yaml and the per-app CRs are already staged, so returning an error dropped
// every one of them before the commit — the obj credential fill and all app toggles
// stopped, every pass, until someone hand-repaired a file on a branch LLZ does not own.
func TestAMalformedTargetDoesNotWedgeTheReconciler(t *testing.T) {
	repo := otomiRepo("6.2.2")
	repo.src[sharedOverlayPath(clusterspec.OverlayObjFile)] = clusterspec.RenderObjOverlayShared()
	repo.src[envOverlayPath("primary", clusterspec.OverlayObjFile)] = clusterspec.RenderObjOverlayEnv("acme", "primary", "us-ord-1")
	repo.tgt[aplOtomiTarget] = "kind: AplCapabilitySet\nspec:\n  version: [oops\n"

	if err := Reconcile(context.Background(), testCfg(), repo, credsOK, metrics.NewRegistry()); err != nil {
		t.Fatalf("a broken file on the machine branch must not fail the pass: %v", err)
	}
	if _, ok := repo.gotFiles[aplOverlayTargets[clusterspec.OverlayObjFile]]; !ok {
		t.Errorf("obj.yaml must still sync while otomi.yaml is unreadable: %v", keysOf(repo.gotFiles))
	}
	if _, ok := repo.gotFiles[aplOtomiTarget]; ok {
		t.Error("the unreadable target must not be written")
	}
}
