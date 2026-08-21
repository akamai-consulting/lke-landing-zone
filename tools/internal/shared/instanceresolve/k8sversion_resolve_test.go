package instanceresolve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// The two accounts measured on 2026-08-11 IN THE SAME HOUR (see
// linode/lke_versions.go). They are the fixture the whole feature turns on: the
// e2e account could not build what the other one offered, so no literal compiled
// into this repo is right for both.
// THE COUPLING GATE FOR #448 IS NOT HERE. It lives in
// extensions/lifecycle/environments/k8sversion_wiring_test.go, because the
// contract it protects runs through the real `llz env add` — resolver, spec
// writer and `llz render` — and a version of it calling this package's functions
// directly would keep passing after the caller stopped calling them. What is here
// is the resolver's own arms.
var (
	e2eAccountCatalog   = []string{"v1.34.6+lke2", "v1.32.9+lke4"}
	otherAccountCatalog = []string{"v1.33.6+lke7"}
)

type fakeVersionLister struct {
	versions []string
	err      error
	calls    int
	// clusters is what /lke/clusters answers, and clusterErr fails that read. Both
	// are on the SAME fake because they are the same account and the same client —
	// see LKEVersionLister.
	clusters         []map[string]any
	clusterErr       error
	failFirst        bool
	clusterCalls     int
	clusterDeadlines []time.Time
}

func (f *fakeVersionLister) ListLKEVersions(_ context.Context, tier string) ([]string, error) {
	f.calls++
	if tier != linode.LKETierEnterprise {
		return nil, errors.New("wrong tier " + tier + ": LKE-E is the only product this landing zone builds")
	}
	return f.versions, f.err
}

func (f *fakeVersionLister) ListClusters(ctx context.Context) ([]map[string]any, error) {
	f.clusterCalls++
	// The deadline each attempt is handed. A retry that shares ONE context across
	// attempts hands out the identical deadline every time, and is decorative: the
	// first slow request spends the budget and the second attempt cannot run.
	if dl, ok := ctx.Deadline(); ok {
		f.clusterDeadlines = append(f.clusterDeadlines, dl)
	}
	// failFirst models a TRANSIENT read — the shape the retry exists for. It fails
	// exactly once and then answers, so a test can tell "retried" from "gave up".
	if f.failFirst && f.clusterCalls == 1 {
		return nil, errors.New("503 service unavailable")
	}
	return f.clusters, f.clusterErr
}

// noClusterReadPause stubs the retry pause. Without it every error-path test pays
// linode.ClusterReadRetryPause of real time for a delay that is not under test.
func noClusterReadPause(t *testing.T) {
	t.Helper()
	prev := clusterReadSleep
	// A MILLISECOND RATHER THAN ZERO, so two attempts are separable on the clock —
	// TestEachClusterReadAttemptGetsItsOwnBudget compares the deadlines they were
	// handed, and a zero-cost stub would make a correct implementation's two
	// deadlines indistinguishable from a shared one's.
	clusterReadSleep = func(time.Duration) { time.Sleep(time.Millisecond) }
	t.Cleanup(func() { clusterReadSleep = prev })
}

// cluster is one row of the account's cluster listing, in the shape
// linode.ClusterVersionFor reads.
func cluster(label, region, version string) map[string]any {
	return map[string]any{"label": label, "region": region, "k8s_version": version}
}

// lab is the deployment every test below is about — an instance called
// `platform-support` adding `lab`, so the label is the one
// envdef.ClusterLabelFor would author.
var lab = Deployment{ClusterLabel: "platform-support-lab", Region: "us-ord"}

// withCatalog installs a fake catalog for one test. A nil lister means "no token".
func withCatalog(t *testing.T, l LKEVersionLister) {
	t.Helper()
	prev := LKEVersionClient
	LKEVersionClient = func() LKEVersionLister { return l }
	resetAccountCheckSkip()
	t.Cleanup(func() { LKEVersionClient = prev; resetAccountCheckSkip() })
}

func TestResolveK8sVersionDerivesTheNewestTheAccountOffers(t *testing.T) {
	f := &fakeVersionLister{versions: e2eAccountCatalog}
	withCatalog(t, f)
	c, err := ResolveK8sVersion("", Deployment{})
	if err != nil {
		t.Fatalf("ResolveK8sVersion: %v", err)
	}
	if c.Newest != "v1.34.6+lke2" {
		t.Errorf("Newest = %q, want v1.34.6+lke2 (the newest of %v)", c.Newest, e2eAccountCatalog)
	}
	if c.Pin != "" {
		t.Errorf("Pin = %q, want \"\" — the operator passed no --k8s-version", c.Pin)
	}
	// The catalog comes back so a caller can judge the INHERITED default without a
	// second request. Nil here is what forced the second read this struct removed.
	if len(c.Offered) != len(e2eAccountCatalog) {
		t.Errorf("Offered = %v, want the catalog itself", c.Offered)
	}
	// SILENT about Newest on purpose: whether it is used is the caller's call, and a
	// note here announced a seeding the second `env add` never performs.
	if c.Note != "" {
		t.Errorf("Note = %q, want silence — only the caller knows if Newest was used", c.Note)
	}
	if f.calls != 1 {
		t.Errorf("asked the account %d times, want exactly 1", f.calls)
	}
	// The SAME code against the OTHER account picks that account's version. This is
	// the assertion a hardcoded default cannot satisfy, and the reason the feature
	// exists.
	withCatalog(t, &fakeVersionLister{versions: otherAccountCatalog})
	if c, _ := ResolveK8sVersion("", Deployment{}); c.Newest != "v1.33.6+lke7" {
		t.Errorf("Newest against the other account = %q, want v1.33.6+lke7", c.Newest)
	}
}

// TestResolveK8sVersionKeepsTheCallersDefaultWhenItCannotAsk is the fail-OPEN half.
// `llz env add` has never needed a token or a network and must not start: every
// unanswerable arm returns ("", "", nil) so envdef keeps its offline fallback.
func TestResolveK8sVersionKeepsTheCallersDefaultWhenItCannotAsk(t *testing.T) {
	for name, lister := range map[string]LKEVersionLister{
		"no token configured":    nil,
		"API error":              &fakeVersionLister{err: errors.New("[401] Invalid Token")},
		"empty catalog":          &fakeVersionLister{versions: []string{}},
		"catalog names no build": &fakeVersionLister{versions: []string{"1.33", "1.34"}},
		"catalog is unparseable": &fakeVersionLister{versions: []string{"latest"}},
	} {
		t.Run(name, func(t *testing.T) {
			withCatalog(t, lister)
			c, err := ResolveK8sVersion("", Deployment{})
			if err != nil {
				t.Fatalf("%s must not fail `llz env add`: %v", name, err)
			}
			if c.Newest != "" {
				t.Errorf("%s derived %q — \"\" is the signal that the caller keeps its own default", name, c.Newest)
			}
			// AND IT MUST NOT OVERRIDE AN INHERITED PIN EITHER. An unread catalog is
			// not evidence that a later deployment's shared default is unbuildable;
			// imposing divergence on no evidence is the opposite failure.
			if repl := c.ReplacementForInheritedPin("v1.33.6+lke7"); repl != "" {
				t.Errorf("%s replaced an inherited pin with %q on no evidence", name, repl)
			}
			// An OPERATOR'S pin survives the same arms untouched. An unanswerable
			// question is not evidence against it.
			if c, err := ResolveK8sVersion("v1.33.6+lke7", Deployment{}); err != nil || c.Pin != "v1.33.6+lke7" {
				t.Errorf("%s: supplied pin = (%q, %v), want it passed through unchanged", name, c.Pin, err)
			}
		})
	}
}

func TestResolveK8sVersionConfirmsASuppliedPinTheAccountOffers(t *testing.T) {
	withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog})
	c, err := ResolveK8sVersion(" v1.32.9+lke4 ", Deployment{})
	if err != nil {
		t.Fatalf("ResolveK8sVersion: %v", err)
	}
	// Trimmed, never otherwise rewritten: terraform sends this string verbatim.
	if c.Pin != "v1.32.9+lke4" {
		t.Errorf("Pin = %q, want v1.32.9+lke4", c.Pin)
	}
	if c.Note == "" {
		t.Error("a confirmed pin should say so — the check is the reason to export LINODE_TOKEN")
	}
	// THE PIN IS NOT THE SHARED DEFAULT. Newest stays the account's answer so
	// `llz env add --k8s-version` cannot decide the version of every deployment
	// added afterwards — the rule --region and --node-type already follow.
	if c.Newest != "v1.34.6+lke2" {
		t.Errorf("Newest = %q, want v1.34.6+lke2 — an explicit pin must not become spec.defaults", c.Newest)
	}
}

func TestResolveK8sVersionRefusesASuppliedPinTheAccountCannotBuild(t *testing.T) {
	withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog})
	_, err := ResolveK8sVersion("v1.33.6+lke7", Deployment{})
	if err == nil {
		t.Fatal("a pin the catalog definitely rejects must fail at `llz env add`, not at `llz doctor` " +
			"~an hour later on a spec the operator has since committed")
	}
	// NAME WHAT IS PRESENT, not only what is missing: the versions the account DOES
	// offer are the whole remedy, and there is no release note that can supply them.
	for _, want := range append([]string{"v1.33.6+lke7"}, e2eAccountCatalog...) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure must name %q; got:\n%s", want, err)
		}
	}
}

func TestResolveK8sVersionNamesTheOneCharacterOnANearMiss(t *testing.T) {
	withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog})
	_, err := ResolveK8sVersion("1.34.6+lke2", Deployment{}) // the leading `v`, which is sent verbatim
	if err == nil {
		t.Fatal("a v-less pin is not in the catalog and the API rejects it as written")
	}
	if !strings.Contains(err.Error(), "v1.34.6+lke2") {
		t.Errorf("a near miss must name the catalog's spelling rather than print the list; got:\n%s", err)
	}
}

func TestResolveK8sVersionDoesNotJudgeAPinAgainstACatalogThatCannotSettleIt(t *testing.T) {
	// A catalog of bare major.minor can neither confirm nor reject a +lke build —
	// linode.CheckVersion returns UNKNOWN, and turning that into a failure is how a
	// scaffold gets blocked on a spelling difference nobody has measured.
	withCatalog(t, &fakeVersionLister{versions: []string{"1.33", "1.34"}})
	c, err := ResolveK8sVersion("v1.33.6+lke7", Deployment{})
	if err != nil || c.Pin != "v1.33.6+lke7" {
		t.Fatalf("got (%q, %v), want the pin passed through with no error", c.Pin, err)
	}
	if c.Note != "" {
		t.Errorf("Note = %q, want silence — an unsettleable catalog must not read as confirmation", c.Note)
	}
	// A catalog that cannot disprove a build id cannot license a divergent override
	// either. Same rule as CheckVersion's: too coarse to reject is too coarse to act.
	if repl := c.ReplacementForInheritedPin("v1.33.6+lke7"); repl != "" {
		t.Errorf("ReplacementForInheritedPin = %q against a catalog that cannot settle it, want \"\"", repl)
	}
}

// TestLKEVersionClientFollowsTheSameTokenDiscoveryAsItsNeighbours pins the one
// thing the fake seam can never exercise: which environment variables make a live
// client exist at all. `llz env add` must degrade to "no check" rather than
// construct a client that 401s on every call, and the two accepted names are the
// same pair RegionClient and ObjClusterClient read — an `llz doctor` that finds a
// token this does not would report a check that silently never runs.
func TestLKEVersionClientFollowsTheSameTokenDiscoveryAsItsNeighbours(t *testing.T) {
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("LINODE_API_TOKEN", "")
	if c := LKEVersionClient(); c != nil {
		t.Errorf("with no token configured the client must be nil (= skip the check), got %T", c)
	}
	for _, envVar := range []string{"LINODE_TOKEN", "LINODE_API_TOKEN"} {
		t.Run(envVar, func(t *testing.T) {
			t.Setenv("LINODE_TOKEN", "")
			t.Setenv("LINODE_API_TOKEN", "")
			t.Setenv(envVar, "a-token")
			if LKEVersionClient() == nil {
				t.Errorf("%s is set but no client was built — the catalog check would never run", envVar)
			}
		})
	}
}

// TestReplacementForInheritedPinOnlyDivergesOnEvidence covers the half a second
// `llz env add` depends on: a deployment added later inherits spec.defaults, which
// nobody re-checked since the instance was scaffolded.
func TestReplacementForInheritedPinOnlyDivergesOnEvidence(t *testing.T) {
	withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog})
	c, err := ResolveK8sVersion("", Deployment{})
	if err != nil {
		t.Fatal(err)
	}
	// The shared default has rotated out — the new deployment cannot be created
	// against it, so it pins the newest for itself.
	if got := c.ReplacementForInheritedPin("v1.33.6+lke7"); got != "v1.34.6+lke2" {
		t.Errorf("ReplacementForInheritedPin(rotated-out) = %q, want v1.34.6+lke2", got)
	}
	// EVERY OTHER ANSWER INHERITS, because divergence between deployments is a real
	// cost this may only impose on a definite negative.
	for name, inherited := range map[string]string{
		"still offered":      "v1.32.9+lke4",
		"already the newest": "v1.34.6+lke2",
		"nothing inherited":  "",
		"blank":              "   ",
	} {
		if got := c.ReplacementForInheritedPin(inherited); got != "" {
			t.Errorf("%s: ReplacementForInheritedPin(%q) = %q, want \"\" (inherit unchanged)", name, inherited, got)
		}
	}
}

// TestRejectingAPinDoesNotAdviseAControlPlaneUpgrade — the remedy for a rejected
// pin depends on something this verb now asks about (#453).
//
// "Omit --k8s-version and llz picks the newest" is right for a deployment that
// does not exist yet, and actively harmful for one being RE-SCAFFOLDED over a live
// cluster: the newest would plan a control-plane upgrade nobody asked for. Before
// #453 this verb made ONE catalog request and never read the account's clusters,
// so the message had to carry a case it could not detect. Now it reads them — and
// the rejection has to say WHICH of the two it found, because "we looked and there
// is no such cluster" and "we did not look" are different claims.
func TestRejectingAPinDoesNotAdviseAControlPlaneUpgrade(t *testing.T) {
	t.Run("a cluster exists and runs something else", func(t *testing.T) {
		withCatalog(t, &fakeVersionLister{
			versions: e2eAccountCatalog,
			clusters: []map[string]any{cluster("platform-support-lab", "us-ord", "v1.32.9+lke4")},
		})
		_, err := ResolveK8sVersion("v1.33.6+lke7", lab)
		if err == nil {
			t.Fatal("expected the rejection")
		}
		for _, want := range []string{
			"platform-support-lab", // names the cluster it looked up
			"v1.32.9+lke4",         // and what that cluster actually runs
			"Omit --k8s-version",   // and the remedy that now pins it automatically
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the rejection must mention %q; got:\n%s", want, err)
			}
		}
	})

	t.Run("no cluster exists", func(t *testing.T) {
		withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog})
		_, err := ResolveK8sVersion("v1.33.6+lke7", lab)
		if err == nil {
			t.Fatal("expected the rejection")
		}
		// IT MUST NOT CLAIM MORE THAN IT CHECKED, and it must not claim less: the
		// account WAS asked, so the message says the cluster is absent rather than
		// leaving the operator to wonder whether an exemption was even considered.
		for _, want := range []string{"No single cluster named", "platform-support-lab"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the rejection must mention %q; got:\n%s", want, err)
			}
		}
		// AND IT MUST NOT OFFER THE ADOPTION IT JUST SAID IS IMPOSSIBLE. This arm used
		// to require the words "ALREADY RUNNING" — the promise that omitting the flag
		// lets llz pin what the cluster runs — in the one message that has just
		// established there is no such cluster. The assertion was pinning the defect.
		if strings.Contains(err.Error(), "ALREADY RUNNING") {
			t.Errorf("the rejection offers to adopt a running version two lines after saying no such "+
				"cluster exists:\n%s", err)
		}
		if !strings.Contains(err.Error(), "no cluster for this deployment to adopt") {
			t.Errorf("the rejection does not say why the adoption path is unavailable:\n%s", err)
		}
	})
}

// TestARescaffoldOverALiveClusterPinsWhatItRuns is the #453 unit arm: the three
// faces of one missing question, at the resolver.
func TestARescaffoldOverALiveClusterPinsWhatItRuns(t *testing.T) {
	// FACE 2 — no pin, and a cluster that already exists. Seeding the newest here is
	// an LKE-Enterprise control-plane upgrade nobody asked for, and
	// `llz ci assert-k8s-version` cannot catch it: the new pin IS in the catalog, it
	// is simply not the one that cluster runs.
	t.Run("no pin adopts the running version", func(t *testing.T) {
		f := &fakeVersionLister{
			versions: e2eAccountCatalog,
			clusters: []map[string]any{cluster("platform-support-lab", "us-ord", "v1.32.9+lke4")},
		}
		withCatalog(t, f)
		c, err := ResolveK8sVersion("", lab)
		if err != nil {
			t.Fatalf("ResolveK8sVersion: %v", err)
		}
		if f.clusterCalls == 0 {
			t.Fatal("the account's clusters were never listed — the whole point of #453")
		}
		if c.Pin != "v1.32.9+lke4" {
			t.Errorf("Pin = %q, want v1.32.9+lke4 (what the cluster runs)", c.Pin)
		}
		if c.Running != "v1.32.9+lke4" {
			t.Errorf("Running = %q, want v1.32.9+lke4", c.Running)
		}
		// spec.defaults is NOT moved: a deployment added to this instance later
		// genuinely should get today's newest, not this cluster's version.
		if c.Newest != "v1.34.6+lke2" {
			t.Errorf("Newest = %q, want v1.34.6+lke2 — the shared default must still be the account's newest", c.Newest)
		}
		if c.Note == "" || !strings.Contains(c.Note, "platform-support-lab") {
			t.Errorf("adopting a version silently is the failure mode; Note = %q", c.Note)
		}
	})

	// FACE 1 — an explicit pin that has rotated out of the catalog, on a cluster
	// that is running it. This is the documented remedy for face 2, and before #453
	// it stopped working the day the version left the catalog (hours, for LKE-E).
	t.Run("an explicit rotated-out pin is exempted when the cluster runs it", func(t *testing.T) {
		withCatalog(t, &fakeVersionLister{
			versions: e2eAccountCatalog,
			clusters: []map[string]any{cluster("platform-support-lab", "us-ord", "v1.33.6+lke7")},
		})
		c, err := ResolveK8sVersion("v1.33.6+lke7", lab)
		if err != nil {
			t.Fatalf("a pin the cluster is already running must not be rejected: %v", err)
		}
		if c.Pin != "v1.33.6+lke7" {
			t.Errorf("Pin = %q, want the pin the operator passed", c.Pin)
		}
		// LOUD, not silent: the deployment is now on a version the account cannot
		// build, so it can no longer be RE-CREATED, and the orphan case is the one way
		// this exemption is wrong.
		for _, want := range []string{"RE-CREATED", "ORPHAN"} {
			if !strings.Contains(c.Warning, want) {
				t.Errorf("the exemption must say %q; Warning = %q", want, c.Warning)
			}
		}
	})

	// THE NEGATIVE ARM, without which "always pin what's running" passes while doing
	// the wrong thing on every fresh instance.
	t.Run("no cluster still gets the account's newest", func(t *testing.T) {
		withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog})
		c, err := ResolveK8sVersion("", lab)
		if err != nil {
			t.Fatalf("ResolveK8sVersion: %v", err)
		}
		if c.Pin != "" {
			t.Errorf("Pin = %q, want \"\" — a fresh deployment has nothing to adopt and inherits spec.defaults", c.Pin)
		}
		if c.Newest != "v1.34.6+lke2" {
			t.Errorf("Newest = %q, want v1.34.6+lke2", c.Newest)
		}
	})

	// AMBIGUITY IS NOT AN ANSWER. Two clusters at one label+region is an account
	// this must not guess about — see linode.ClusterVersionFor.
	t.Run("an ambiguous account changes nothing", func(t *testing.T) {
		withCatalog(t, &fakeVersionLister{
			versions: e2eAccountCatalog,
			clusters: []map[string]any{
				cluster("platform-support-lab", "us-ord", "v1.32.9+lke4"),
				cluster("platform-support-lab", "us-ord", "v1.33.6+lke7"),
			},
		})
		c, err := ResolveK8sVersion("", lab)
		if err != nil {
			t.Fatalf("ResolveK8sVersion: %v", err)
		}
		if c.Pin != "" || c.Running != "" {
			t.Errorf("Pin = %q, Running = %q — two matches must fall through to today's behaviour", c.Pin, c.Running)
		}
	})

	// THE REGION IS PART OF THE MATCH, exactly as it is for the preflight: a
	// same-named cluster in a different region is a different deployment.
	t.Run("a cluster in another region is not this deployment", func(t *testing.T) {
		withCatalog(t, &fakeVersionLister{
			versions: e2eAccountCatalog,
			clusters: []map[string]any{cluster("platform-support-lab", "de-fra-2", "v1.32.9+lke4")},
		})
		c, err := ResolveK8sVersion("", lab)
		if err != nil {
			t.Fatalf("ResolveK8sVersion: %v", err)
		}
		if c.Pin != "" {
			t.Errorf("Pin = %q, want \"\"", c.Pin)
		}
	})
}

// TestTheClusterReadIsBestEffortAndSaysSoWhenItFails.
//
// `llz env add` has never needed a token or a network and must not start. But the
// fallback for a failed read is "seed today's newest", which against a live cluster
// is precisely the unrequested upgrade the read exists to prevent — so degrading
// QUIETLY is the one thing it may not do.
func TestTheClusterReadIsBestEffortAndSaysSoWhenItFails(t *testing.T) {
	noClusterReadPause(t)
	withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog, clusterErr: errors.New("503 service unavailable")})
	out := captureStderr(t, func() {
		c, err := ResolveK8sVersion("", lab)
		if err != nil {
			t.Fatalf("a failed cluster read must not fail the scaffold: %v", err)
		}
		if c.Newest != "v1.34.6+lke2" {
			t.Errorf("Newest = %q, want the catalog answer to survive", c.Newest)
		}
	})
	for _, want := range []string{"platform-support-lab", "503", "control-plane upgrade"} {
		if !strings.Contains(out, want) {
			t.Errorf("a failed cluster read must say %q; stderr was:\n%s", want, out)
		}
	}
}

// TestAFailedClusterReadDoesNotClaimTheAccountWasLookedAt.
//
// A failed read and an account with no such cluster both leave nothing to adopt,
// and they license OPPOSITE sentences. The first cut of the rejection said
// `No single cluster named "…" is on this account` off a 503 — a claim llz had
// never verified, in the message that then failed the operator's build. The
// verdict is unchanged (an unreadable cluster list is not evidence that a cluster
// exists — the preflight's own rule); what changes is that llz says which it is.
func TestAFailedClusterReadDoesNotClaimTheAccountWasLookedAt(t *testing.T) {
	noClusterReadPause(t)
	withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog, clusterErr: errors.New("503 service unavailable")})
	_, err := ResolveK8sVersion("v1.33.6+lke7", lab)
	if err == nil {
		t.Fatal("an unprovable exemption must not discharge a proven-bad pin")
	}
	if strings.Contains(err.Error(), "No single cluster named") {
		t.Errorf("the rejection claims the account was checked after the read FAILED:\n%s", err)
	}
	for _, want := range []string{"could not be checked", "the catalog verdict stands", "transient"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the rejection must say %q so a re-run is understood to be worth it; got:\n%s", want, err)
		}
	}
}

// TestATransientClusterReadDoesNotDecideADeploymentIsUnbuildable.
//
// The exemption is a PROOF that terraform will not send the pin, so one 503 must
// not be what decides a running deployment is unbuildable — the same reasoning
// linode.ClusterReadAttempts encodes for the preflight and `llz doctor`. An
// earlier revision of this file read once, which turned a blip into a hard failure
// on exactly the remedy `llz ci assert-k8s-version` recommends.
func TestATransientClusterReadDoesNotDecideADeploymentIsUnbuildable(t *testing.T) {
	noClusterReadPause(t)
	f := &fakeVersionLister{
		versions:  e2eAccountCatalog,
		failFirst: true,
		clusters:  []map[string]any{cluster("platform-support-lab", "us-ord", "v1.33.6+lke7")},
	}
	withCatalog(t, f)
	c, err := ResolveK8sVersion("v1.33.6+lke7", lab)
	if err != nil {
		t.Fatalf("one transient 503 failed a pin the cluster is already running: %v", err)
	}
	if c.Running != "v1.33.6+lke7" {
		t.Errorf("Running = %q, want the retried read's answer", c.Running)
	}
	if f.clusterCalls != 2 {
		t.Errorf("listed the account's clusters %d time(s), want 2 (linode.ClusterReadAttempts)", f.clusterCalls)
	}
}

// TestTheClusterReadRetryIsBounded — a bounded retry, not a spin. The budget is
// linode.ClusterReadAttempts, shared with the two callers that already read this
// route so the three cannot drift apart.
func TestTheClusterReadRetryIsBounded(t *testing.T) {
	noClusterReadPause(t)
	f := &fakeVersionLister{versions: e2eAccountCatalog, clusterErr: errors.New("503 service unavailable")}
	withCatalog(t, f)
	if _, err := ResolveK8sVersion("", lab); err != nil {
		t.Fatalf("a failed cluster read must not fail the scaffold: %v", err)
	}
	if f.clusterCalls != linode.ClusterReadAttempts {
		t.Errorf("listed the account's clusters %d time(s), want linode.ClusterReadAttempts (%d)",
			f.clusterCalls, linode.ClusterReadAttempts)
	}
}

// TestEachClusterReadAttemptGetsItsOwnBudget.
//
// THE RETRY WAS DECORATIVE FOR ONE REVISION AND NOTHING NOTICED. A single
// context.WithTimeout wrapped the whole loop while the HTTP client's own timeout
// was the same duration, so a SLOW (rather than fast-failing) request exhausted the
// shared budget on attempt 1, tripped the `ctx.Err() != nil` break, and attempt 2
// never ran — falling back to seeding Newest over a live cluster, or hard-failing a
// --k8s-version the cluster is actually on. Every other test here uses a fake that
// answers instantly, so the deadline is the only thing that distinguishes the two
// shapes: a shared context hands out the SAME deadline twice.
//
// The two callers that already read this route carve a per-attempt sub-budget for
// exactly this reason (verbs/doctor, assertplatform).
func TestEachClusterReadAttemptGetsItsOwnBudget(t *testing.T) {
	noClusterReadPause(t)
	f := &fakeVersionLister{
		versions:  e2eAccountCatalog,
		failFirst: true,
		clusters:  []map[string]any{cluster("platform-support-lab", "us-ord", "v1.33.6+lke7")},
	}
	withCatalog(t, f)
	if _, err := ResolveK8sVersion("", lab); err != nil {
		t.Fatalf("ResolveK8sVersion: %v", err)
	}
	if len(f.clusterDeadlines) != 2 {
		t.Fatalf("saw %d attempt deadline(s), want 2 — the retry did not run", len(f.clusterDeadlines))
	}
	if !f.clusterDeadlines[1].After(f.clusterDeadlines[0]) {
		t.Errorf("both attempts were handed the same deadline (%v), so they SHARE one budget: a slow\n"+
			"first request spends it and the retry cannot fire. Each attempt needs its own sub-budget\n"+
			"carved out of the probe budget.", f.clusterDeadlines[0])
	}
}

// TestTheProbeBudgetOutlastsTheAttemptsItIsMeantToHold — a sizing invariant, not a
// behaviour. If the parent budget were not strictly larger than what the attempts
// plus their pauses can consume, the last attempt would be cut off by the parent
// and the retry would again be decorative. Asserted rather than commented, because
// the four constants are edited independently.
func TestTheProbeBudgetOutlastsTheAttemptsItIsMeantToHold(t *testing.T) {
	need := time.Duration(linode.ClusterReadAttempts)*accountReadBudget +
		time.Duration(linode.ClusterReadAttempts)*linode.ClusterReadRetryPause
	if clusterProbeTimeout <= need {
		t.Errorf("clusterProbeTimeout = %v, which cannot hold %d attempt(s) of %v plus their pauses (%v)",
			clusterProbeTimeout, linode.ClusterReadAttempts, accountReadBudget, need)
	}
}

// TestTheAdoptionMessagesAreTrueBEFOREAnythingIsWritten.
//
// These strings are printed on the `--dry-run` path too, where nothing is written.
// The first cut said "pinned it" and "can no longer be RE-CREATED" — past-tense
// claims about a write that never happened, which is the exact defect
// printK8sVersionConsequences was extracted to fix for the other two consequences.
func TestTheAdoptionMessagesAreTrueBEFOREAnythingIsWritten(t *testing.T) {
	for name, tc := range map[string]struct{ catalog, want string }{
		"still offered": {"v1.32.9+lke4", "pinning it"},
		"rotated out":   {"v1.33.6+lke7", "pinning it anyway"},
	} {
		withCatalog(t, &fakeVersionLister{
			versions: e2eAccountCatalog,
			clusters: []map[string]any{cluster("platform-support-lab", "us-ord", tc.catalog)},
		})
		c, err := ResolveK8sVersion("", lab)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := c.Note + c.Warning
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: the message must describe the DECISION, not report a completed write "+
				"(it is printed under --dry-run too); want %q in:\n%s", name, tc.want, got)
		}
		if strings.Contains(got, "pinned it") {
			t.Errorf("%s: past tense about a write that has not happened yet:\n%s", name, got)
		}
		if strings.Contains(got, "can no longer be") {
			t.Errorf("%s: asserts a state the run may never create (--dry-run):\n%s", name, got)
		}
	}
}

// TestAnAmbiguousAccountIsNotSilent.
//
// ClusterVersionFor answers "" for BOTH zero matches and several, and only one of
// those is uneventful. Several clusters at one label+region is the shape a failed
// cycle leaves — an orphan sitting beside the live deployment — and falling through
// to "seed today's newest" there plans a control-plane upgrade on whichever one is
// real. The file's own rule is that this fallback never happens quietly; it was
// written for the failed-READ arm and not upheld for the ambiguous-MATCH arm.
func TestAnAmbiguousAccountIsNotSilent(t *testing.T) {
	withCatalog(t, &fakeVersionLister{
		versions: e2eAccountCatalog,
		clusters: []map[string]any{
			cluster("platform-support-lab", "us-ord", "v1.32.9+lke4"),
			cluster("platform-support-lab", "us-ord", "v1.33.6+lke7"),
		},
	})
	var c K8sVersionChoice
	var err error
	out := captureStderr(t, func() { c, err = ResolveK8sVersion("", lab) })
	if err != nil {
		t.Fatalf("an ambiguous account must not fail the scaffold: %v", err)
	}
	// THE VERDICT IS UNCHANGED — this must not start guessing. Only the silence goes.
	if c.Pin != "" || c.Running != "" {
		t.Errorf("Pin = %q, Running = %q — two matches must not be guessed between", c.Pin, c.Running)
	}
	for _, want := range []string{"platform-support-lab", "cannot tell which one", "control-plane"} {
		if !strings.Contains(out, want) {
			t.Errorf("an ambiguous account fell back to seeding the newest without saying %q; stderr:\n%s", want, out)
		}
	}
}

// TestAnAccountWithNoMatchIsQuiet is the negative arm: "warn whenever nothing was
// adopted" would pass the test above while shouting at every fresh instance, which
// is most of them.
func TestAnAccountWithNoMatchIsQuiet(t *testing.T) {
	withCatalog(t, &fakeVersionLister{
		versions: e2eAccountCatalog,
		clusters: []map[string]any{cluster("platform-support-dr", "us-ord", "v1.32.9+lke4")},
	})
	out := captureStderr(t, func() {
		if _, err := ResolveK8sVersion("", lab); err != nil {
			t.Fatalf("ResolveK8sVersion: %v", err)
		}
	})
	if strings.Contains(out, "cannot tell which one") {
		t.Errorf("a deployment with no cluster is the ORDINARY case and must not be warned about:\n%s", out)
	}
}

// TestTheAmbiguityRemedyDoesNotDESTROYTheLiveDeployment.
//
// THE FIRST CUT OF THIS WARNING TOLD OPERATORS TO RUN `llz reap --cluster-label
// <label>`. That flag is LABEL-scoped: it lists every cluster carrying the label
// and DELETEs each one. This warning fires precisely BECAUSE two clusters share the
// label — so the advice, followed with --yes, destroys the live deployment
// alongside the orphan it was meant to sweep. A remedy that is catastrophic in the
// exact state that prints it is worse than no remedy.
func TestTheAmbiguityRemedyDoesNotDESTROYTheLiveDeployment(t *testing.T) {
	withCatalog(t, &fakeVersionLister{
		versions: e2eAccountCatalog,
		// float64 ids, NOT Go ints — that is what encoding/json produces for a number,
		// and linode.MatchClusterIDs decodes json.Number and float64 only. A fixture
		// written with int literals decodes to zero and would have hidden both the
		// decode and the formatting bug this test exists for. The second id is seven
		// digits on purpose: %v on a float64 that size prints `1.234567e+06`, an id
		// nobody can paste into a delete command.
		clusters: []map[string]any{
			{"id": float64(111), "label": "platform-support-lab", "region": "us-ord", "k8s_version": "v1.32.9+lke4"},
			{"id": float64(1234567), "label": "platform-support-lab", "region": "us-ord", "k8s_version": "v1.33.6+lke7"},
		},
	})
	out := captureStderr(t, func() {
		if _, err := ResolveK8sVersion("", lab); err != nil {
			t.Fatalf("ResolveK8sVersion: %v", err)
		}
	})
	// IT MUST NAME THE IDS, because telling someone two clusters collide without
	// saying which is which leaves them no way to act that is not a guess.
	for _, want := range []string{"111", "1234567", "v1.32.9+lke4", "v1.33.6+lke7"} {
		if !strings.Contains(out, want) {
			t.Errorf("the ambiguity warning does not name %q, so the operator cannot tell the two "+
				"clusters apart; stderr:\n%s", want, out)
		}
	}
	// AND IT MUST WARN OFF the label-scoped sweep rather than recommend it.
	if !strings.Contains(out, "would delete both") {
		t.Errorf("the warning does not say that `llz reap --cluster-label` is label-scoped and takes "+
			"BOTH clusters; stderr:\n%s", out)
	}
}

// TestACoarsePinIsNotWavedThroughInSilence.
//
// CheckVersion confirms on a byte-exact match, so `--k8s-version 1.33` against a
// MIXED catalog holding a coarse `1.33` row comes back Offered. For one revision
// llz merely withheld its confirmation note — and said nothing at all. Every gate
// this feature added is then GREEN (doctor and assert-k8s-version make the same
// byte-exact match), the spec carries the pin, and the apply is what discovers the
// problem ~15 minutes in: the exact failure the feature exists to remove, wearing a
// full set of passing checks.
func TestACoarsePinIsNotWavedThroughInSilence(t *testing.T) {
	mixed := []string{"v1.34.6+lke2", "1.33"}
	withCatalog(t, &fakeVersionLister{versions: mixed})
	c, err := ResolveK8sVersion("1.33", lab)
	// A WARNING, NOT A REJECTION. CheckVersion owns the verdict, and #443 is explicit
	// that a coarse catalog is an unmeasured shape rather than a falsehood — nothing
	// here may become the stricter of two checks that are supposed to be one.
	if err != nil {
		t.Fatalf("a coarse pin must not be REJECTED here — the catalog is not entitled to: %v", err)
	}
	if c.Note != "" {
		t.Errorf("llz confirmed a pin the create API rejects: %q", c.Note)
	}
	for _, want := range []string{"not a full LKE-E build id", "VERBATIM", "assert-k8s-version"} {
		if !strings.Contains(c.Warning, want) {
			t.Errorf("a coarse pin was accepted in silence; Warning must say %q, got:\n%s", want, c.Warning)
		}
	}
}

// TestABuildIdPinIsStillJustConfirmed is the negative arm: warning on every
// Offered pin would pass the test above while nagging about every correct one.
func TestABuildIdPinIsStillJustConfirmed(t *testing.T) {
	withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog})
	c, err := ResolveK8sVersion("v1.34.6+lke2", lab)
	if err != nil {
		t.Fatalf("ResolveK8sVersion: %v", err)
	}
	if c.Note == "" {
		t.Error("a full build id the account offers must still be confirmed")
	}
	if c.Warning != "" {
		t.Errorf("a correct pin was warned about:\n%s", c.Warning)
	}
}

// TestAClusterThatWillNotSayWhatItRunsIsNotNoClusterAtAll.
//
// Exactly one cluster matched, and it reports no k8s_version. Until this case
// existed that was indistinguishable from "the account holds no such cluster": llz
// seeded the newest over a live deployment in silence, and the rejection message
// then told the operator that no cluster of that name was on the account — about a
// listing that contained precisely one.
func TestAClusterThatWillNotSayWhatItRunsIsNotNoClusterAtAll(t *testing.T) {
	// A cluster row with no k8s_version at all, which is what the API returns while
	// one is still provisioning.
	silent := []map[string]any{{"id": float64(4242), "label": "platform-support-lab", "region": "us-ord"}}

	t.Run("it says so instead of seeding in silence", func(t *testing.T) {
		withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog, clusters: silent})
		var c K8sVersionChoice
		out := captureStderr(t, func() {
			var err error
			if c, err = ResolveK8sVersion("", lab); err != nil {
				t.Fatalf("ResolveK8sVersion: %v", err)
			}
		})
		if c.Pin != "" {
			t.Errorf("Pin = %q — there is no version to adopt from a cluster that reports none", c.Pin)
		}
		for _, want := range []string{"4242", "reports no k8s_version", "control-plane upgrade"} {
			if !strings.Contains(out, want) {
				t.Errorf("a live cluster llz could not read was passed over in silence; want %q in:\n%s", want, out)
			}
		}
	})

	t.Run("and the rejection does not claim the cluster is absent", func(t *testing.T) {
		withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog, clusters: silent})
		var err error
		captureStderr(t, func() { _, err = ResolveK8sVersion("v1.33.6+lke7", lab) })
		if err == nil {
			t.Fatal("expected the rejection — nothing proved this pin is exempt")
		}
		if strings.Contains(err.Error(), "No single cluster named") {
			t.Errorf("the rejection says no such cluster exists, about a listing holding one:\n%s", err)
		}
		if !strings.Contains(err.Error(), "4242") {
			t.Errorf("the rejection does not name the cluster it did find:\n%s", err)
		}
	})
}

// TestTheFailedReadWarningNamesNoFallbackItCannotKnow.
//
// One warning, three callers, three different fallbacks: the catalog was read (seed
// the newest), the catalog was NOT read (seed llz's compiled literal), or an
// explicit pin is about to be REJECTED and nothing is seeded at all. Asserting "llz
// falls back to seeding the newest version this account offers" was false in two of
// the three — and printed in the same run as the message saying what actually
// happened.
func TestTheFailedReadWarningNamesNoFallbackItCannotKnow(t *testing.T) {
	noClusterReadPause(t)
	withCatalog(t, &fakeVersionLister{err: errors.New("401 unauthorized"), clusterErr: errors.New("503")})
	out := captureStderr(t, func() {
		if _, err := ResolveK8sVersion("", lab); err != nil {
			t.Fatalf("ResolveK8sVersion: %v", err)
		}
	})
	if !strings.Contains(out, "could not be checked") {
		t.Fatalf("the failed-read warning did not fire:\n%s", out)
	}
	// The catalog ALSO failed here, so the seed is llz's compiled literal — the one
	// thing the warning must not call "the newest version this account offers".
	if strings.Contains(out, "seeding the newest version this account offers") {
		t.Errorf("the warning names a fallback that is not the one this run will take:\n%s", out)
	}
}

// TestTheOrphanRemedyIsScopedToTheClusterItMatched.
//
// `llz reap --cluster-label` sweeps by label ACCOUNT-WIDE — ClustersWithLabel does
// not filter by region — while the adoption matched label AND region. So a
// same-labelled cluster in another region is a live deployment the advice would
// delete. Round 5 removed this hazard from the ambiguity warning; it survived in
// adoptionMessage because the two were written a round apart.
func TestTheOrphanRemedyIsScopedToTheClusterItMatched(t *testing.T) {
	withCatalog(t, &fakeVersionLister{
		versions: e2eAccountCatalog,
		clusters: []map[string]any{
			{"id": float64(7654321), "label": "platform-support-lab", "region": "us-ord", "k8s_version": "v1.33.6+lke7"},
			// The live peer in ANOTHER region that a label-scoped sweep would take.
			{"id": float64(9999), "label": "platform-support-lab", "region": "de-fra-2", "k8s_version": "v1.34.6+lke2"},
		},
	})
	c, err := ResolveK8sVersion("", lab)
	if err != nil {
		t.Fatalf("ResolveK8sVersion: %v", err)
	}
	if c.Pin != "v1.33.6+lke7" {
		t.Fatalf("premise broken: region must narrow the match to one cluster; Pin = %q", c.Pin)
	}
	if !strings.Contains(c.Warning, "7654321") {
		t.Errorf("the orphan remedy does not name the id it matched, so it cannot be acted on safely:\n%s", c.Warning)
	}
	if !strings.Contains(c.Warning, "sweeps every region") {
		t.Errorf("the orphan remedy does not warn that `llz reap --cluster-label` is region-blind and "+
			"would take live deployments sharing the label:\n%s", c.Warning)
	}
}

// TestAnEmptyCatalogIsNotReportedAsAnUNREACHABLEAccount.
//
// A read that SUCCEEDED and listed nothing was routed through
// reportSkippedAccountCheck, whose error arm reads "the API did not answer" and
// tells the operator their token is probably expired, revoked or under-scoped.
// Their token worked. And in the same run k8sVersionBanner correctly reported the
// catalog as READ — so llz made two contradictory statements about one request and
// sent the operator off to re-mint a PAT that was fine.
func TestAnEmptyCatalogIsNotReportedAsAnUNREACHABLEAccount(t *testing.T) {
	withCatalog(t, &fakeVersionLister{versions: nil})
	var c K8sVersionChoice
	out := captureStderr(t, func() {
		var err error
		if c, err = ResolveK8sVersion("", Deployment{}); err != nil {
			t.Fatalf("ResolveK8sVersion: %v", err)
		}
	})
	// THE FLAG THAT KEEPS THE BANNER AND THE NOTICE ON ONE STORY.
	if c.Catalog != CatalogAnswered {
		t.Error("the read succeeded, so Catalog must be CatalogAnswered — the notice and the banner key on it")
	}
	if strings.Contains(out, "did not answer") || strings.Contains(out, "expired") {
		t.Errorf("an account that ANSWERED was reported as unreachable, blaming a working token:\n%s", out)
	}
	if !strings.Contains(out, "lists NO LKE-Enterprise versions") {
		t.Errorf("the empty answer was not reported as what it is:\n%s", out)
	}
}

// TestTheSkipNoticeNamesTheFieldItDecidesNotAFlagNobodyPassed.
//
// This derivation runs on EVERY `llz env add`, flag or no flag. Naming
// `--k8s-version` told an operator who never passed one that it "was NOT checked" —
// a sentence about something they did not do, which reads as a mistake they made.
// `cluster.k8sVersion` is what is actually being decided, and it is the field they
// will go and look at.
func TestTheSkipNoticeNamesTheFieldItDecidesNotAFlagNobodyPassed(t *testing.T) {
	withCatalog(t, &fakeVersionLister{err: errors.New("401 unauthorized")})
	out := captureStderr(t, func() {
		if _, err := ResolveK8sVersion("", Deployment{}); err != nil {
			t.Fatalf("ResolveK8sVersion: %v", err)
		}
	})
	if !strings.Contains(out, "cluster.k8sVersion") {
		t.Errorf("the notice does not name the spec field it decides:\n%s", out)
	}
	if strings.Contains(out, "--k8s-version was NOT checked") {
		t.Errorf("the notice blames a flag the operator never passed:\n%s", out)
	}
}

// TestAskedAndRefusedIsNotNeverAsked.
//
// The catalog outcome began as one bool, and a bool cannot hold this: "no token" and
// "asked, and the API refused" both read as not-answered. So a token whose versions
// route 401s produced "the API did not answer" from the skip notice and "this
// account was never asked" from the seed provenance — one run, one request, two
// contradictory claims, with different remedies attached (fix the token you have,
// versus export one).
func TestAskedAndRefusedIsNotNeverAsked(t *testing.T) {
	for name, tc := range map[string]struct {
		lister *fakeVersionLister
		want   CatalogOutcome
	}{
		"no token":            {nil, CatalogNotAsked},
		"the API refused":     {&fakeVersionLister{err: errors.New("401 unauthorized")}, CatalogFailed},
		"answered with none":  {&fakeVersionLister{versions: nil}, CatalogAnswered},
		"answered but coarse": {&fakeVersionLister{versions: []string{"1.33"}}, CatalogAnswered},
		"answered properly":   {&fakeVersionLister{versions: e2eAccountCatalog}, CatalogAnswered},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.lister == nil {
				withCatalog(t, nil)
			} else {
				withCatalog(t, tc.lister)
			}
			var c K8sVersionChoice
			captureStderr(t, func() {
				var err error
				if c, err = ResolveK8sVersion("", Deployment{}); err != nil {
					t.Fatalf("ResolveK8sVersion: %v", err)
				}
			})
			if c.Catalog != tc.want {
				t.Errorf("Catalog = %v, want %v — the three outcomes license three different "+
					"sentences and three different remedies", c.Catalog, tc.want)
			}
		})
	}
}

// TestTheEmptyCatalogWarningDoesNotPREDICTThePin.
//
// It fires from accountLKEVersions, BEFORE the pin is decided, and three things can
// still happen after it: a live cluster is adopted and Pin becomes what it runs, an
// explicit --k8s-version is written as given, or a later `env add` inherits
// spec.defaults and falls back to nothing at all. The first cut said "the spec keeps
// its compiled default" — contradicting, from 400 lines up, the sibling warning
// whose own comment spells out this exact rule.
func TestTheEmptyCatalogWarningDoesNotPREDICTThePin(t *testing.T) {
	// The case that most obviously falsifies the old claim: an empty catalog AND a
	// live cluster, so the pin llz writes is the adopted one, not any default.
	withCatalog(t, &fakeVersionLister{
		versions: nil,
		clusters: []map[string]any{cluster("platform-support-lab", "us-ord", "v1.33.6+lke7")},
	})
	var c K8sVersionChoice
	out := captureStderr(t, func() {
		var err error
		if c, err = ResolveK8sVersion("", lab); err != nil {
			t.Fatalf("ResolveK8sVersion: %v", err)
		}
	})
	if c.Pin != "v1.33.6+lke7" {
		t.Fatalf("premise broken: Pin = %q, want the adopted version", c.Pin)
	}
	if !strings.Contains(out, "lists NO LKE-Enterprise versions") {
		t.Fatalf("the empty-catalog warning did not fire:\n%s", out)
	}
	if strings.Contains(out, "keeps its compiled default") {
		t.Errorf("the warning predicts a compiled default while llz went on to pin %q from a live "+
			"cluster — it runs BEFORE the pin is decided and cannot know:\n%s", c.Pin, out)
	}
}

// TestTheCoarsePinWarningExplainsTheRuleItActuallyAPPLIES.
//
// The warning tells the operator WHY their pin is not a full build id, and that
// explanation has to match the predicate that rejected it. It did not: NamesABuild
// was tightened to the same parse NewestVersion uses (MAJOR.MINOR.PATCH + `+lkeN`)
// while the warning still explained itself with the old substring rule — "those
// carry an `+lke` suffix". So a pin of `v1.34+lke2`, which HAS an `+lke` suffix, was
// told it lacked one. That is the same contradiction class this arm was added to
// remove, one message along.
//
// The fixture is the exact shape the linode-side coupling test names as the split
// between the two rules, so this cannot pass by accident.
func TestTheCoarsePinWarningExplainsTheRuleItActuallyAPPLIES(t *testing.T) {
	const pin = "v1.34+lke2" // a build suffix, no patch component
	if linode.NamesABuild(pin) {
		t.Fatalf("premise broken: %q must be rejected by NamesABuild for this arm to fire", pin)
	}
	withCatalog(t, &fakeVersionLister{versions: []string{pin}})
	c, err := ResolveK8sVersion(pin, lab)
	if err != nil {
		t.Fatalf("a coarse pin is warned about, not rejected: %v", err)
	}
	if c.Warning == "" {
		t.Fatal("the coarse-pin warning did not fire")
	}
	// THE EXPLANATION MUST NOT BE FALSIFIED BY THE PIN IT DESCRIBES. Asserting the
	// absence of the old rule rather than the presence of new wording, so this stays
	// a correctness gate and not a spellcheck.
	if strings.Contains(c.Warning, "carry an `+lke` suffix") {
		t.Errorf("the warning tells the operator a build id is one that carries `+lke` — which %q does, "+
			"and llz rejected it anyway:\n%s", pin, c.Warning)
	}
	if !strings.Contains(c.Warning, "PATCH") {
		t.Errorf("the warning does not name the component %q actually lacks, so the operator cannot "+
			"tell what to change:\n%s", pin, c.Warning)
	}
}

// TestCatalogOutcomeNamesItselfInAFailure.
//
// The names ARE this type's point — each state licenses a different sentence and a
// different remedy — and a bare int defeats that exactly where it matters most.
// TestAskedAndRefusedIsNotNeverAsked exists to prove the three stay
// distinguishable, and it was reporting `Catalog = 1, want 2`: no reader can tell
// WHICH distinction broke from the failure message of the test whose whole subject
// is that distinction.
func TestCatalogOutcomeNamesItselfInAFailure(t *testing.T) {
	for want, o := range map[string]CatalogOutcome{
		"CatalogNotAsked": CatalogNotAsked,
		"CatalogFailed":   CatalogFailed,
		"CatalogAnswered": CatalogAnswered,
	} {
		if got := fmt.Sprintf("%v", o); got != want {
			t.Errorf("CatalogOutcome(%d) renders as %q, want %q — a failure message that prints an "+
				"integer cannot say which state llz got", int(o), got, want)
		}
	}
	// AN UNKNOWN VALUE STILL SAYS WHAT IT IS rather than rendering as an empty
	// string, which is how a stringer turns an out-of-range bug into a silent one.
	if got := fmt.Sprintf("%v", CatalogOutcome(99)); !strings.Contains(got, "99") {
		t.Errorf("an out-of-range CatalogOutcome rendered as %q, losing the value", got)
	}
}

// TestAHardRejectionDoesNotPromiseADeriveThatYieldsNothing.
//
// TWO RULES DECIDE DIFFERENT HALVES OF THIS MESSAGE, ON PURPOSE, and the gap
// between them is a real catalog shape. The catalog's entitlement to REJECT runs
// off the loose `hasBuild` (via everyEntryNamesABuild); what llz would CHOOSE runs
// off the strict versionSortKey. A catalog of `["v1.34+lke2"]` licenses the
// rejection and derives "" — so the remedy said "omit it and llz picks the newest
// your account offers" in a run that had ALREADY printed "this catalog names no
// full build id", and omitting would have fallen through to the compiled literal.
//
// The asymmetry stays: each direction is the safe one for its own question. What a
// remedy may not do is promise what this catalog cannot deliver.
func TestAHardRejectionDoesNotPromiseADeriveThatYieldsNothing(t *testing.T) {
	// The shape where the two rules split — a build suffix with no patch component.
	unusable := []string{"v1.34+lke2"}
	if linode.NamesABuild(unusable[0]) {
		t.Fatalf("premise broken: %q must not be choosable", unusable[0])
	}
	withCatalog(t, &fakeVersionLister{versions: unusable})
	_, err := ResolveK8sVersion("v1.33.6+lke7", lab)
	if err == nil {
		t.Fatal("expected the rejection — this catalog is entitled to reject")
	}
	if strings.Contains(err.Error(), "the newest this account offers") {
		t.Errorf("the rejection tells the operator to omit the flag and let llz derive, from a catalog\n"+
			"that derives NOTHING:\n%s", err)
	}
	for _, want := range []string{"no version for llz to derive", "llz doctor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the rejection must say %q so the operator is not sent round a loop; got:\n%s", want, err)
		}
	}
	// AND IT NAMES NO SINGLE DESTINATION. This package cannot know where the version
	// comes from once the flag is dropped — only the FIRST `llz env add` seeds
	// anything, a later one inherits spec.defaults — so a message that picks one is
	// the predict-the-pin defect this branch removed from two other warnings and then
	// reintroduced here.
	if strings.Contains(err.Error(), "fall back to its compiled default") {
		t.Errorf("the rejection predicts a compiled default, but on an instance that already has\n"+
			"landingzone.yaml the deployment inherits spec.defaults instead:\n%s", err)
	}

	// THE NEGATIVE ARM: a catalog that CAN derive must still offer the remedy, or
	// "never promise a derive" passes the above while withholding the fix from every
	// ordinary rejection.
	withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog})
	_, err = ResolveK8sVersion("v1.33.6+lke7", lab)
	if err == nil {
		t.Fatal("expected the rejection")
	}
	if !strings.Contains(err.Error(), "the newest this account offers") {
		t.Errorf("a catalog that CAN derive must still be offered as the remedy:\n%s", err)
	}
	// The same no-single-destination rule on this branch, which carried the defect
	// first and was only noticed after the other one was written.
	if !strings.Contains(err.Error(), "spec.defaults") {
		t.Errorf("the remedy names only a first-`env add` outcome; a later one inherits the shared\n"+
			"default and the message must say so:\n%s", err)
	}
}

// TestTheOmitRemedyOffersAdoptionONLYWhenTheLookupCouldDeliverIt.
//
// Every arm that reaches omitRemedy has an EMPTY lk.Running — the exemption and the
// runs-something-else error both return earlier — so "omit it and llz pins what
// your cluster is ALREADY RUNNING, which plans no diff at all" was never true of
// the run printing it. On the ambiguous arm it directly contradicted the warning
// classifyClusters had emitted seconds before, in the same run: llz cannot tell
// which cluster is this deployment's and will not guess.
//
// The four states are separated because they need four different next actions:
// resolve the duplicate, read the version by hand, re-run, or accept there is
// nothing to adopt.
func TestTheOmitRemedyOffersAdoptionONLYWhenTheLookupCouldDeliverIt(t *testing.T) {
	const label = "platform-support-lab"
	for name, tc := range map[string]struct {
		lister  *fakeVersionLister
		want    string
		mustNot string
	}{
		"ambiguous": {
			&fakeVersionLister{versions: e2eAccountCatalog, clusters: []map[string]any{
				{"id": float64(1), "label": label, "region": "us-ord", "k8s_version": "v1.32.9+lke4"},
				{"id": float64(2), "label": label, "region": "us-ord", "k8s_version": "v1.34.6+lke2"},
			}},
			"will not guess between them", "no diff at all",
		},
		"unreadable": {
			&fakeVersionLister{versions: e2eAccountCatalog, clusters: []map[string]any{
				{"id": float64(3), "label": label, "region": "us-ord"},
			}},
			"reports none for that cluster", "no diff at all",
		},
		"read failed": {
			&fakeVersionLister{versions: e2eAccountCatalog, clusterErr: errors.New("503")},
			"A re-run may settle it", "",
		},
		"no cluster": {
			&fakeVersionLister{versions: e2eAccountCatalog},
			"no cluster for this deployment to adopt", "no diff at all",
		},
	} {
		t.Run(name, func(t *testing.T) {
			noClusterReadPause(t)
			withCatalog(t, tc.lister)
			var err error
			captureStderr(t, func() { _, err = ResolveK8sVersion("v1.33.6+lke7", lab) })
			if err == nil {
				t.Fatal("expected the rejection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the remedy does not say %q, so the operator is not told what to do next:\n%s",
					tc.want, err)
			}
			// "plans no diff at all" is the adoption promise. It may appear ONLY where
			// the lookup could actually deliver it — which, on the failed-read arm, is
			// phrased as a maybe rather than withheld.
			if tc.mustNot != "" && strings.Contains(err.Error(), tc.mustNot) {
				t.Errorf("the remedy promises the adoption path on a run whose lookup cannot deliver "+
					"it (%s):\n%s", name, err)
			}
			// AND THE REJECTION ITSELF IS THE FACT THE OPERATOR GUIDANCE RESTS ON. The
			// e2e prose used to say `vars.E2E_K8S_VERSION` is the escape for the states
			// where llz cannot answer — but in every one of these the catalog WAS read,
			// so an explicit pin is judged against it with no exemption to grant, and
			// hard-fails the moment that version rotates out. Only the no-token case is
			// safe, because there nothing judges the pin at all. That distinction is
			// three prose sites deep and this is where it is actually decided.
			if !strings.Contains(err.Error(), "is not an LKE-Enterprise version this account can build") {
				t.Errorf("%s: an explicit pin must be REJECTED here — the guidance about when "+
					"E2E_K8S_VERSION is a durable escape depends on it:\n%s", name, err)
			}
		})
	}
}

// TestAnUnusableCatalogStillLeavesTheAdoptionDoorOpen.
//
// ADOPTION NEEDS NO CATALOG — it copies a version off a running cluster — so "omit
// it and llz will NOT rescue this" is false when the CLUSTER read is the thing that
// failed. The same error had already told the operator to re-run if that read was
// transient, and a re-run that succeeds would in fact adopt.
func TestAnUnusableCatalogStillLeavesTheAdoptionDoorOpen(t *testing.T) {
	noClusterReadPause(t)
	// A catalog that can supply nothing AND a cluster read that failed.
	withCatalog(t, &fakeVersionLister{versions: []string{"v1.34+lke2"}, clusterErr: errors.New("503")})
	var err error
	captureStderr(t, func() { _, err = ResolveK8sVersion("v1.33.6+lke7", lab) })
	if err == nil {
		t.Fatal("expected the rejection")
	}
	if !strings.Contains(err.Error(), "ONE EXCEPTION") {
		t.Errorf("the remedy declares the door shut while the CLUSTER read — which needs no catalog —\n"+
			"is the thing that failed, in the same error that says to re-run:\n%s", err)
	}

	// THE NEGATIVE ARM: when the read SUCCEEDED and found nothing, there is no
	// exception to offer, and pretending otherwise sends the operator round a loop.
	withCatalog(t, &fakeVersionLister{versions: []string{"v1.34+lke2"}})
	captureStderr(t, func() { _, err = ResolveK8sVersion("v1.33.6+lke7", lab) })
	if err == nil {
		t.Fatal("expected the rejection")
	}
	if strings.Contains(err.Error(), "ONE EXCEPTION") {
		t.Errorf("the read succeeded and found no cluster, so a re-run changes nothing:\n%s", err)
	}
}

// TestAZeroDeploymentIsNotToldAboutAClusterOrARead.
//
// A zero Deployment is documented as DISABLING the read, and !lk.Asked covers both
// "the read failed" and "there was nothing to look up" — only the first has a read
// above to point at. Unguarded, the remedy cited "that cluster" and "the read
// above" for a run that had neither. Latent, since `llz env add` always supplies a
// label; pinned because the sibling lookedUp case is guarded and these two must not
// drift apart.
func TestAZeroDeploymentIsNotToldAboutAClusterOrARead(t *testing.T) {
	withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog})
	_, err := ResolveK8sVersion("v1.33.6+lke7", Deployment{})
	if err == nil {
		t.Fatal("expected the rejection")
	}
	for _, forbidden := range []string{"that cluster", "the read above", "A re-run may settle it"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("no read happened and no label was given, but the remedy says %q:\n%s", forbidden, err)
		}
	}
}

// TestAConfirmedPinCostsNoClusterRead pins the cheapness rule. --k8s-version is
// the operator saying the version out loud and the catalog agreeing; there is
// nothing left for an exemption or an adoption to decide, so the second request
// must not be made.
func TestAConfirmedPinCostsNoClusterRead(t *testing.T) {
	f := &fakeVersionLister{
		versions: e2eAccountCatalog,
		clusters: []map[string]any{cluster("platform-support-lab", "us-ord", "v1.32.9+lke4")},
	}
	withCatalog(t, f)
	if _, err := ResolveK8sVersion("v1.34.6+lke2", lab); err != nil {
		t.Fatalf("ResolveK8sVersion: %v", err)
	}
	if f.clusterCalls != 0 {
		t.Errorf("listed the account's clusters %d time(s) for a pin the catalog already confirmed", f.clusterCalls)
	}
}

// TestAnUnreadableCatalogDoesNotDISABLETheClusterRead.
//
// THIS TEST USED TO ASSERT THE OPPOSITE, and the reasoning it encoded was wrong.
// It said a catalog that could not be answered means no token or no reachable API,
// so the second request would fail the same way — which collapses three different
// failures into one, and two of them are measured shapes in this repo:
//
//   - the two endpoints are NOT the same endpoint. #426 recorded the e2e token
//     401ing on the VERSIONS route from some contexts while the clusters route
//     answers; they are different paths with different PAT scopes.
//   - an EMPTY catalog lands on the same branch, and an account listing no LKE-E
//     versions says nothing whatever about whether it has clusters.
//
// The cost of the old behaviour was the entire feature: a re-scaffold over a live
// cluster silently re-seeded today's newest and planned the upgrade #453 exists to
// prevent, with nothing left on disk to warn from.
func TestAnUnreadableCatalogDoesNotDISABLETheClusterRead(t *testing.T) {
	f := &fakeVersionLister{
		err:      errors.New("401 unauthorized"),
		clusters: []map[string]any{cluster("platform-support-lab", "us-ord", "v1.33.6+lke7")},
	}
	withCatalog(t, f)
	c, err := ResolveK8sVersion("", lab)
	if err != nil {
		t.Fatalf("ResolveK8sVersion: %v", err)
	}
	if f.clusterCalls == 0 {
		t.Fatal("a 401 on the VERSIONS route disabled the CLUSTER read, so a re-scaffold over a live " +
			"cluster is invisible again — the whole of #453, undone by an unrelated endpoint")
	}
	if c.Pin != "v1.33.6+lke7" {
		t.Errorf("Pin = %q, want the version the cluster runs — adopting it needs no catalog", c.Pin)
	}
	// AND IT CLAIMS NOTHING ABOUT A CATALOG IT COULD NOT READ.
	if strings.Contains(c.Note+c.Warning, "no longer offers") {
		t.Errorf("llz judged the running version against a catalog it never read:\n%s", c.Note+c.Warning)
	}
	if c.Newest != "" {
		t.Errorf("Newest = %q, want \"\" — the caller must keep its own offline default", c.Newest)
	}
}

// TestNoTokenCostsNoRequestsAtAll is the cheapness half the test above must not
// cost: with no client there is nothing to ask, on either endpoint.
func TestNoTokenCostsNoRequestsAtAll(t *testing.T) {
	withCatalog(t, nil)
	c, err := ResolveK8sVersion("", lab)
	if err != nil {
		t.Fatalf("ResolveK8sVersion: %v", err)
	}
	if c.Pin != "" || c.Running != "" || c.Newest != "" {
		t.Errorf("with no token llz decided something: Pin=%q Running=%q Newest=%q", c.Pin, c.Running, c.Newest)
	}
}

// TestAConfirmationIsOnlyClaimedForAPinTheCreateAPICanTake.
//
// CheckVersion confirms on a byte-exact match, which is right for the VERDICT and
// not enough for the CLAIM. Against a catalog holding a bare `1.33` row, a pin of
// `1.33` comes back Offered — and terraform sends cluster.k8sVersion verbatim, so
// the LKE-E create API rejects it.
//
// The first cut of this guard tested c.Newest — "does the catalog name a build
// anywhere" — which reads like the same question and is not: against a MIXED
// catalog it is non-empty, so a coarse pin still got a confident confirmation.
func TestAConfirmationIsOnlyClaimedForAPinTheCreateAPICanTake(t *testing.T) {
	// THE MIXED CATALOG IS THE FIXTURE THAT SEPARATES THE TWO RULES. A purely coarse
	// one has Newest == "" and would pass under either, so it cannot tell them apart.
	withCatalog(t, &fakeVersionLister{versions: []string{"v1.34.6+lke2", "1.33"}})
	c, err := ResolveK8sVersion("1.33", Deployment{})
	if err != nil {
		t.Fatalf("a coarse catalog cannot settle this pin, so it must not fail: %v", err)
	}
	if c.Pin != "1.33" {
		t.Errorf("Pin = %q, want it passed through untouched", c.Pin)
	}
	if c.Note != "" {
		t.Errorf("llz claimed %q about a pin the create API rejects — the catalog matched it, "+
			"but `1.33` is not a string terraform can send", c.Note)
	}
	// A real build id against the same catalog still gets its confirmation, or the
	// guard has simply silenced the useful case too.
	if c, _ := ResolveK8sVersion("v1.34.6+lke2", Deployment{}); c.Note == "" {
		t.Error("a full build id present in the catalog must still be confirmed")
	}
}

// TestAnAnsweredCatalogIsDistinguishableFromAnUnaskedOne — the producer half of a
// distinction len(Offered) cannot make.
//
// A read that FAILED and a read that returned an EMPTY catalog both leave Offered
// nil, and they license opposite sentences. `llz env add` reported the seed's
// origin off len(Offered) and therefore told accounts that had answered that they
// "were never asked" — a claim llz had not verified, printed as the explanation for
// what it just wrote. Same class as the cluster-read arm this file already fixed.
func TestAnAnsweredCatalogIsDistinguishableFromAnUnaskedOne(t *testing.T) {
	noClusterReadPause(t)

	t.Run("the account answered, with nothing in it", func(t *testing.T) {
		withCatalog(t, &fakeVersionLister{versions: nil})
		c, err := ResolveK8sVersion("", lab)
		if err != nil {
			t.Fatalf("an empty catalog must not fail the scaffold: %v", err)
		}
		if c.Catalog != CatalogAnswered {
			t.Error("the catalog read succeeded, so Catalog must be CatalogAnswered — an empty answer IS " +
				"an answer, and reporting it as 'never asked' is a claim llz never verified")
		}
	})

	t.Run("the read failed", func(t *testing.T) {
		withCatalog(t, &fakeVersionLister{err: errors.New("401 Invalid Token")})
		c, err := ResolveK8sVersion("", lab)
		if err != nil {
			t.Fatalf("an unreadable catalog must not fail the scaffold: %v", err)
		}
		if c.Catalog == CatalogAnswered {
			t.Error("the read failed, so Catalog must not be CatalogAnswered — otherwise the field is decorative " +
				"and the caller is back to guessing from len(Offered)")
		}
	})
}
