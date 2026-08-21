package instanceresolve

import (
	"context"
	"errors"
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
	clusters     []map[string]any
	clusterErr   error
	failFirst    bool
	clusterCalls int
}

func (f *fakeVersionLister) ListLKEVersions(_ context.Context, tier string) ([]string, error) {
	f.calls++
	if tier != linode.LKETierEnterprise {
		return nil, errors.New("wrong tier " + tier + ": LKE-E is the only product this landing zone builds")
	}
	return f.versions, f.err
}

func (f *fakeVersionLister) ListClusters(context.Context) ([]map[string]any, error) {
	f.clusterCalls++
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
	clusterReadSleep = func(time.Duration) {}
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
		for _, want := range []string{"No single cluster named", "platform-support-lab", "ALREADY RUNNING"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the rejection must mention %q; got:\n%s", want, err)
			}
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

// TestNoCatalogMeansNoClusterRead. An unanswerable catalog means no token or no
// reachable API; a second request would fail the same way and add a second
// paragraph of the same news.
func TestNoCatalogMeansNoClusterRead(t *testing.T) {
	f := &fakeVersionLister{err: errors.New("401 unauthorized")}
	withCatalog(t, f)
	if _, err := ResolveK8sVersion("", lab); err != nil {
		t.Fatalf("ResolveK8sVersion: %v", err)
	}
	if f.clusterCalls != 0 {
		t.Errorf("listed the account's clusters %d time(s) after the catalog read had already failed", f.clusterCalls)
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
