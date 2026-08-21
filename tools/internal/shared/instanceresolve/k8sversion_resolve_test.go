package instanceresolve

import (
	"context"
	"errors"
	"strings"
	"testing"

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
}

func (f *fakeVersionLister) ListLKEVersions(_ context.Context, tier string) ([]string, error) {
	f.calls++
	if tier != linode.LKETierEnterprise {
		return nil, errors.New("wrong tier " + tier + ": LKE-E is the only product this landing zone builds")
	}
	return f.versions, f.err
}

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
	c, err := ResolveK8sVersion("")
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
	if c, _ := ResolveK8sVersion(""); c.Newest != "v1.33.6+lke7" {
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
			c, err := ResolveK8sVersion("")
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
			if c, err := ResolveK8sVersion("v1.33.6+lke7"); err != nil || c.Pin != "v1.33.6+lke7" {
				t.Errorf("%s: supplied pin = (%q, %v), want it passed through unchanged", name, c.Pin, err)
			}
		})
	}
}

func TestResolveK8sVersionConfirmsASuppliedPinTheAccountOffers(t *testing.T) {
	withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog})
	c, err := ResolveK8sVersion(" v1.32.9+lke4 ")
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
	_, err := ResolveK8sVersion("v1.33.6+lke7")
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
	_, err := ResolveK8sVersion("1.34.6+lke2") // the leading `v`, which is sent verbatim
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
	c, err := ResolveK8sVersion("v1.33.6+lke7")
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
	c, err := ResolveK8sVersion("")
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
// pin depends on something this verb deliberately does not ask about.
//
// "Omit --k8s-version and llz picks the newest" is right for a deployment that
// does not exist yet, and actively harmful for one being RE-SCAFFOLDED over a live
// cluster: the operator is passing the version their cluster is already running,
// and the newest would plan a control-plane upgrade nobody asked for. The
// preflight exempts that cluster (linode.ClusterRunsVersion); this verb makes one
// catalog request and never reads the account's clusters, so the message has to
// carry the case it cannot detect.
func TestRejectingAPinDoesNotAdviseAControlPlaneUpgrade(t *testing.T) {
	withCatalog(t, &fakeVersionLister{versions: e2eAccountCatalog})
	_, err := ResolveK8sVersion("v1.33.6+lke7")
	if err == nil {
		t.Fatal("expected the rejection")
	}
	msg := err.Error()
	for _, want := range []string{
		"ALREADY RUNS",       // names the case
		"environments/",      // and where to put the version back
		"plans no diff",      // and why that is safe
		"assert-k8s-version", // and what will then let it through
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the rejection must mention %q so it is not read as advice to upgrade a live cluster; got:\n%s", want, msg)
		}
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
	c, err := ResolveK8sVersion("1.33")
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
	if c, _ := ResolveK8sVersion("v1.34.6+lke2"); c.Note == "" {
		t.Error("a full build id present in the catalog must still be confirmed")
	}
}
