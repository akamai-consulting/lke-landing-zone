package assertplatform

// k8s_version_test.go — the two directions this gate has to get right, and they
// pull against each other.
//
// It must FAIL a pin the account cannot build (otherwise it is a print statement,
// and the pin reaches terraform apply as it did on 2026-08-11). And it must PASS
// everything it could not settle — an unreachable API, an under-scoped token, an
// empty or unrecognised catalog — because the alternative is a Linode blip failing
// a build on a perfectly good spec, and nobody trusts a gate that does that twice.
//
// So the negative arms are not padding here; they are the load-bearing half.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// theE2EAccount is what /v4beta/lke/tiers/enterprise/versions actually returned on
// 2026-08-11, in the same hour another account still offered v1.33.6+lke7.
var theE2EAccount = []string{"v1.34.6+lke2", "v1.32.9+lke4"}

// THE INCIDENT. A pin this account cannot build must fail HERE, in seconds,
// rather than ~15 minutes into the cluster apply — after the VPC, object-storage
// and database roots have already applied.
func TestK8sVersionVerdictFailsAPinTheAccountCannotBuild(t *testing.T) {
	_, err := k8sVersionVerdict("prod", "v1.33.6+lke7", false, theE2EAccount, nil)
	if err == nil {
		t.Fatal("a pin absent from the account's catalog must fail the preflight — " +
			"passing here is exactly how it reached `[400] [k8s_version] k8s_version is not valid`")
	}
	msg := err.Error()
	// Naming what IS offered is most of the value: the operator's next action is
	// choosing one, and the list is per-account so they cannot look it up anywhere.
	for _, want := range []string{"v1.33.6+lke7", "v1.34.6+lke2", "v1.32.9+lke4", "prod", "PER-ACCOUNT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the failure must mention %q so the fix is in the message; got:\n%s", want, msg)
		}
	}
}

func TestK8sVersionVerdictPassesAPinTheAccountOffers(t *testing.T) {
	p, err := k8sVersionVerdict("prod", "v1.34.6+lke2", false, theE2EAccount, nil)
	if err != nil {
		t.Fatalf("an offered pin must pass: %v", err)
	}
	if !strings.Contains(p.note, "v1.34.6+lke2") {
		t.Errorf("the success line should name the version it checked; got: %s", p.note)
	}
	if p.kind != k8sOffered {
		t.Errorf("kind = %q, want %q — the record is what says this run decided anything", p.kind, k8sOffered)
	}
}

// THE FAIL-OPEN HALF. Every one of these is "the question could not be answered",
// and the issue that asked for this gate is explicit that none of them may block:
// the e2e token 401s on the versions route from some contexts.
func TestK8sVersionVerdictWarnsAndPassesOnAnUnanswerableQuestion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		want    string
		offered []string
		err     error
	}{
		{"api unreachable / token unscoped", "v1.33.6+lke7", nil, errors.New("GET /v4beta/lke/tiers/enterprise/versions returned 401: Invalid Token")},
		{"no token configured", "v1.33.6+lke7", nil, errors.New("set LINODE_TOKEN (or LINODE_API_TOKEN) to a Linode PAT")},
		{"empty catalog", "v1.33.6+lke7", nil, nil},
		{"catalog too coarse to disprove the pin", "v1.33.6+lke7", []string{"1.30", "1.31"}, nil},
	} {
		p, err := k8sVersionVerdict("prod", tc.want, false, tc.offered, tc.err)
		if err != nil {
			t.Errorf("%s: returned an error (%v) — an unanswerable question must warn and PASS, "+
				"or a Linode blip fails a build on a good spec", tc.name, err)
		}
		if strings.TrimSpace(p.note) == "" {
			t.Errorf("%s: passed silently — a check that skipped must say so, or it is "+
				"indistinguishable from one that verified something", tc.name)
		}
		// AND IT MUST RECORD ITSELF AS UNDECIDED. This is the arm issue #449 is about:
		// a pass here looks identical to a real one in the exit status, so the only
		// thing that can tell an inert pipeline from a working one is the record.
		if p.kind != k8sUndecided {
			t.Errorf("%s: kind = %q, want %q — a soft pass that records itself as a decision "+
				"is the green check the class hides behind", tc.name, p.kind, k8sUndecided)
		}
		if p.reason == "" {
			t.Errorf("%s: recorded UNDECIDED with no reason — \"it could not be answered\" and "+
				"WHY are different findings and only one of them is actionable", tc.name)
		}
	}
}

// A COARSE CATALOG CANNOT SETTLE A BUILD PIN IN EITHER DIRECTION, so the lane must
// neither fail on it nor claim a pass. Both arms are UNCHECKED, and the one that
// used to read "is offered" is the one that mattered: an unqualified pass for a
// retired build, followed by the apply dying at ~15 min with the 400.
func TestK8sVersionVerdictOnACoarseCatalog(t *testing.T) {
	for _, offered := range [][]string{{"1.33"}, {"1.30", "1.31"}} {
		p, err := k8sVersionVerdict("prod", "v1.33.6+lke7", false, offered, nil)
		if err != nil {
			t.Fatalf("a catalog that cannot express builds must not REJECT one (%v): %v", offered, err)
		}
		if !strings.Contains(p.note, "UNCHECKED") {
			t.Errorf("%v cannot confirm a +lke build either; claiming a pass is how an operator "+
				"stops looking: %s", offered, p.note)
		}
	}

	// A pin the coarse catalog literally lists IS confirmed — an exact match is an
	// exact match whatever precision the list is written at.
	p, err := k8sVersionVerdict("prod", "1.33", false, []string{"1.33"}, nil)
	if err != nil || strings.Contains(p.note, "UNCHECKED") {
		t.Errorf("an exact match is a pass at any precision (err=%v): %s", err, p.note)
	}
}

// ── the wiring around the decision ───────────────────────────────────────────

func withSpec(t *testing.T, lz *clusterspec.LandingZone, present bool, err error) {
	t.Helper()
	prev := deps
	t.Cleanup(func() { deps = prev })
	deps.LoadSpec = func() (*clusterspec.LandingZone, bool, error) { return lz, present, err }
}

func withLister(t *testing.T, f func(context.Context) ([]string, error)) {
	t.Helper()
	prev := listAccountLKEVersions
	t.Cleanup(func() { listAccountLKEVersions = prev })
	listAccountLKEVersions = f
	// Default the cluster read to "no clusters" so a test that does not care about
	// the exemption gets the fresh-account shape rather than a live API call.
	withClusters(t, func(context.Context) ([]map[string]any, error) { return nil, nil })
}

func withClusters(t *testing.T, f func(context.Context) ([]map[string]any, error)) {
	t.Helper()
	prev := listAccountClusters
	t.Cleanup(func() { listAccountClusters = prev })
	listAccountClusters = f
}

func lkeCluster(label, region, version string) map[string]any {
	return map[string]any{"label": label, "region": region, "k8s_version": version}
}

func specPinning(env, version string) *clusterspec.LandingZone {
	lz := &clusterspec.LandingZone{}
	lz.Spec.Environments = map[string]clusterspec.Environment{
		env: {Cluster: clusterspec.Cluster{K8sVersion: version, ClusterLabel: "llz-" + env, Region: "us-ord"}},
	}
	return lz
}

// A repo with no spec has nothing to check — and must not reach for a Linode
// token to discover that. `llz ci` verbs run in the template repo and on machines
// that have none. This is the ONLY silent-pass arm.
func TestAssertK8sVersionSkipsOutsideAnInstance(t *testing.T) {
	withSpec(t, nil, false, nil)
	called := false
	withLister(t, func(context.Context) ([]string, error) {
		called = true
		return nil, nil
	})
	if err := assertK8sVersion("prod"); err != nil {
		t.Errorf("outside an instance there is nothing to check: %v", err)
	}
	if called {
		t.Error("asked the Linode API with no spec to check — a verb that reaches for a token " +
			"it does not need fails wherever there is none")
	}
}

// FAIL CLOSED ON THE SIDE IT OWNS. Every one of these used to print
// "no k8sVersion pinned … nothing to check" and exit 0, which is the gate going
// dead while its output claims it looked. Since cluster.k8sVersion is REQUIRED on
// every deployment the spec defines, an unknown --env was the only way that arm
// could fire in practice — so the quiet path was, in production, exactly the
// mis-invocation path.
func TestAssertK8sVersionRefusesToPassHavingExaminedNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lz       *clusterspec.LandingZone
		env      string
		specErr  error
		wantText string
	}{
		{"--env names no deployment", specPinning("lab", "v1.34.6+lke2"), "prod", nil, "checked NOTHING"},
		{"--env is empty", specPinning("lab", "v1.34.6+lke2"), "", nil, "checked NOTHING"},
		{"deployment pins nothing", specPinning("prod", "   "), "prod", nil, "required field"},
		{"the spec will not parse", nil, "prod", errors.New("yaml: line 4: mapping values not allowed"), "UNCHECKED"},
	} {
		withSpec(t, tc.lz, tc.lz != nil, tc.specErr)
		called := false
		withLister(t, func(context.Context) ([]string, error) {
			called = true
			return theE2EAccount, nil
		})
		err := assertK8sVersion(tc.env)
		if err == nil {
			t.Errorf("%s: passed having examined nothing — that is indistinguishable from a "+
				"preflight that verified something", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantText) {
			t.Errorf("%s: error should contain %q; got: %v", tc.name, tc.wantText, err)
		}
		if called {
			t.Errorf("%s: asked the Linode API before it had a pin to ask about", tc.name)
		}
	}
}

// The deployments the spec DOES define are named, because "no such deployment" is
// most often a typo and the fix is in the list.
func TestUnknownDeploymentNamesTheOnesThatExist(t *testing.T) {
	withSpec(t, specPinning("lab", "v1.34.6+lke2"), true, nil)
	err := assertK8sVersion("prd")
	if err == nil || !strings.Contains(err.Error(), "lab") {
		t.Errorf("the refusal should name the deployments the spec defines; got: %v", err)
	}
}

func TestAssertK8sVersionChecksTheDeploymentsOwnPin(t *testing.T) {
	withSpec(t, specPinning("prod", "v1.33.6+lke7"), true, nil)
	withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
	err := assertK8sVersion("prod")
	if err == nil {
		t.Fatal("the spec's pin is not in the account's catalog; the preflight must fail")
	}
	if !strings.Contains(err.Error(), "v1.33.6+lke7") {
		t.Errorf("the error should quote the pin it read from the spec; got: %v", err)
	}
}

// The probe must carry a deadline. Without one a wedged Linode API turns a
// seconds-long preflight into a job that burns its whole timeout — which is worse
// than the ~15-minute apply this exists to save.
func TestAssertK8sVersionBoundsTheProbe(t *testing.T) {
	withSpec(t, specPinning("prod", "v1.34.6+lke2"), true, nil)
	var hasDeadline bool
	withLister(t, func(ctx context.Context) ([]string, error) {
		_, hasDeadline = ctx.Deadline()
		return theE2EAccount, nil
	})
	if err := assertK8sVersion("prod"); err != nil {
		t.Fatalf("an offered pin must pass: %v", err)
	}
	if !hasDeadline {
		t.Error("the version probe ran with no deadline")
	}
}

// The lane LOOKS at an account. Its binding is what stops it doing anything else,
// and a widened grant line is the one edit that would silently undo that.
func TestK8sVersionBindingMayOnlyRead(t *testing.T) {
	h := capability.CloudFor(k8sVersionBinding())
	if err := h.Permits(http.MethodGet); err != nil {
		t.Errorf("the lane must be able to list versions: %v", err)
	}
	for _, m := range []string{http.MethodDelete, http.MethodPost, http.MethodPut} {
		if err := h.Permits(m); err == nil {
			t.Errorf("the binding permits %s — a preflight that could delete the cluster it is "+
				"checking is the anomaly this extension refuses", m)
		}
	}
	var found bool
	for _, g := range k8sVersionBinding().Grants {
		if g == extension.CloudMutate {
			found = true
		}
	}
	if found {
		t.Error("k8s-version declares cloud-mutate")
	}
}

// ── the existing-cluster exemption ───────────────────────────────────────────
//
// THE GATE HAS TO KNOW WHAT TERRAFORM WILL ACTUALLY SEND. `k8s_version` goes to
// the API on create, or on an update that changes it — never for a cluster
// already at the pin. LKE-E versions rotate out of an account's catalog inside an
// hour (measured), so without this the FIRST rotation would block every routine
// apply to a long-lived deployment: apply-cluster needs apply-vpc, so a node-pool
// resize or an ACL change becomes impossible until someone bumps
// cluster.k8sVersion — a control-plane upgrade nobody asked for.

func TestARunningClusterAtThePinIsNotBlockedByARotatedCatalog(t *testing.T) {
	withSpec(t, specPinning("prod", "v1.33.6+lke7"), true, nil)
	withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
	withClusters(t, func(context.Context) ([]map[string]any, error) {
		return []map[string]any{lkeCluster("llz-prod", "us-ord", "v1.33.6+lke7")}, nil
	})
	if err := assertK8sVersion("prod"); err != nil {
		t.Fatalf("the cluster already runs the pin, so terraform plans no change to k8s_version "+
			"and this apply is unaffected — failing here blocks every routine apply once the "+
			"version rotates out: %v", err)
	}
}

// The incident case must still fail: nothing is running the pin, so terraform
// really will hand this string to the API.
func TestNoClusterMeansThePinMustBeBuildable(t *testing.T) {
	for name, clusters := range map[string][]map[string]any{
		"no clusters at all":        nil,
		"a different deployment":    {lkeCluster("llz-lab", "us-ord", "v1.33.6+lke7")},
		"a different region":        {lkeCluster("llz-prod", "us-sea", "v1.33.6+lke7")},
		"same cluster, other build": {lkeCluster("llz-prod", "us-ord", "v1.32.9+lke4")},
		"ambiguous — two matches": {
			lkeCluster("llz-prod", "us-ord", "v1.33.6+lke7"),
			lkeCluster("llz-prod", "us-ord", "v1.33.6+lke7"),
		},
	} {
		withSpec(t, specPinning("prod", "v1.33.6+lke7"), true, nil)
		withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
		withClusters(t, func(context.Context) ([]map[string]any, error) { return clusters, nil })
		if err := assertK8sVersion("prod"); err == nil {
			t.Errorf("%s: nothing on the account is running this pin at this label+region, so "+
				"terraform will send it and the apply dies 15 minutes in", name)
		}
	}
}

// The exemption is only reached on the failing path, so a good build pays for one
// request rather than two.
func TestTheClusterReadOnlyHappensOnTheFailingPath(t *testing.T) {
	withSpec(t, specPinning("prod", "v1.34.6+lke2"), true, nil)
	withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
	asked := false
	withClusters(t, func(context.Context) ([]map[string]any, error) {
		asked = true
		return nil, nil
	})
	if err := assertK8sVersion("prod"); err != nil {
		t.Fatalf("an offered pin must pass: %v", err)
	}
	if asked {
		t.Error("listed the account's clusters for a pin that is plainly offered — the exemption " +
			"is a last question before failing, not a second request on every good build")
	}
}

// AN UNPROVABLE EXEMPTION DOES NOT DISCHARGE A PROVEN FAILURE, and this arm had
// it backwards. "Uncertainty never blocks" is about whether the pin is buildable —
// and by the time this runs that question is ANSWERED: the catalog read succeeded
// and the pin is not in it. All that is in doubt is an escape hatch, and an
// unreadable cluster list is not evidence that a cluster exists.
//
// Returning nil here reproduced the original incident exactly: retired pin, no
// cluster yet, one 503 on /lke/clusters, and the apply dies fifteen minutes in.
func TestAnUnreadableClusterListLeavesTheVerdictStanding(t *testing.T) {
	withSpec(t, specPinning("prod", "v1.33.6+lke7"), true, nil)
	withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
	withClusters(t, func(context.Context) ([]map[string]any, error) {
		return nil, errors.New("503 Service Unavailable")
	})
	err := assertK8sVersion("prod")
	if err == nil {
		t.Fatal("the catalog already disproved the pin; a failed exemption lookup is not a reprieve")
	}
	if !strings.Contains(err.Error(), "v1.33.6+lke7") {
		t.Errorf("the standing verdict should still be the catalog one; got: %v", err)
	}
}

// THE PROBE BUDGET HAS TO COVER EVERY READ IT ALLOWS, and the arithmetic is easy
// to get wrong by adding a call and not the time for it. When the exemption was
// added, the probe budget stayed at 30s against a 20s per-request timeout — so a
// slow catalog read would leave the cluster read to be cancelled by the parent
// context, and the gate would report UNCHECKED instead of a verdict under exactly
// the API slowness it is most likely to meet.
//
// ASSERTED AS ARITHMETIC, AND THE FIRST VERSION OF THIS TEST WAS NOT. It watched
// the deadline at the second call — which reads like a stronger, behavioural check
// and is in fact a weaker one: the stubbed catalog read returns instantly, so the
// full budget is always left and the assertion passed at the buggy 30s value too.
// A test that cannot fail on the regression it names is worse than no test,
// because it retires the question.
//
// The WORST CASE is what the budget has to cover, and only the constants describe
// it — every read spending its whole per-request timeout, plus the pauses between
// retries.
func TestTheProbeBudgetCoversEveryReadItAllows(t *testing.T) {
	worst := (1+linode.ClusterReadAttempts)*k8sVersionReadBudget +
		linode.ClusterReadAttempts*linode.ClusterReadRetryPause
	if k8sVersionProbeTimeout < worst {
		t.Errorf("probe budget %v cannot cover one catalog read plus %d exemption attempt(s) at "+
			"%v each with %v pauses (%v) — under real API slowness the last read is cancelled by "+
			"the parent context and the gate reports UNCHECKED instead of a verdict",
			k8sVersionProbeTimeout, linode.ClusterReadAttempts, k8sVersionReadBudget,
			linode.ClusterReadRetryPause, worst)
	}
}

// A READ IS NOT A REQUEST, and the budget has to be derived from the read.
// http.Client.Timeout bounds one HTTP call while both reads go through
// listAllPages, which issues one per 100-item page — so an account big enough to
// paginate blew through a probe budget sized from the per-request timeout, the
// exemption read got cancelled, and an apply failed for a deployment whose cluster
// was running the pin. (The first version of the arithmetic test asserted that
// same under-counting formula, so it could not have caught it.)
func TestTheReadBudgetAllowsMoreThanOneRequest(t *testing.T) {
	if k8sVersionReadBudget <= k8sVersionRequestTimeout {
		t.Errorf("a read is allowed %v against a per-request timeout of %v — listAllPages issues "+
			"one request per 100-item page, so a single-request budget cuts off any account that "+
			"paginates", k8sVersionReadBudget, k8sVersionRequestTimeout)
	}
}

// Separately: the reads must be bounded at all, or a wedged API turns a
// seconds-long preflight into a job that burns its whole timeout.
func TestTheExemptionReadIsBounded(t *testing.T) {
	withSpec(t, specPinning("prod", "v1.33.6+lke7"), true, nil)
	withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
	var ok bool
	withClusters(t, func(ctx context.Context) ([]map[string]any, error) {
		_, ok = ctx.Deadline()
		return nil, nil
	})
	_ = assertK8sVersion("prod")
	if !ok {
		t.Fatal("the cluster read ran with no deadline")
	}
}

// THE REMEDIATION HAS TO NAME THE FILE THE PIN IS ACTUALLY IN. It used to say
// `environments/<env>.yaml` unconditionally, but landingzone.yaml.example seeds
// the pin in spec.defaults — where EVERY deployment inherits it. Following that
// advice unblocks one deployment and leaves the real stale value un-named, so the
// rest fail one dispatch at a time.
func TestTheFixNamesWhereThePinActuallyLives(t *testing.T) {
	_, own := k8sVersionVerdict("prod", "v1.33.6+lke7", false, theE2EAccount, nil)
	if own == nil {
		t.Fatal("the pin is not offered; this must fail")
	}
	if !strings.Contains(own.Error(), "environments/prod.yaml") || strings.Contains(own.Error(), "landingzone.yaml") {
		t.Errorf("a deployment-specific pin is fixed in its own file; got:\n%s", own)
	}

	_, inherited := k8sVersionVerdict("prod", "v1.33.6+lke7", true, theE2EAccount, nil)
	if inherited == nil {
		t.Fatal("the pin is not offered; this must fail")
	}
	for _, want := range []string{"landingzone.yaml", "spec.defaults", "environments/prod.yaml"} {
		if !strings.Contains(inherited.Error(), want) {
			t.Errorf("an inherited pin must name %q — both the shared value and the per-deployment "+
				"override, or the operator fixes the wrong one; got:\n%s", want, inherited)
		}
	}
}

// specK8sVersion is what decides which of those two messages fires, so the
// inheritance detection is worth pinning on its own.
func TestSharedPinsAreRecognisedAsInherited(t *testing.T) {
	lz := specPinning("prod", "v1.33.6+lke7")
	lz.Spec.Defaults.Cluster.K8sVersion = "v1.33.6+lke7"
	withSpec(t, lz, true, nil)
	if _, _, _, shared, _, err := specK8sVersion("prod"); err != nil || !shared {
		t.Errorf("a pin equal to spec.defaults is one landingzone.yaml plausibly owns (shared=%v, err=%v)", shared, err)
	}

	lz = specPinning("prod", "v1.33.6+lke7")
	lz.Spec.Defaults.Cluster.K8sVersion = "v1.34.6+lke2"
	withSpec(t, lz, true, nil)
	if _, _, _, shared, _, err := specK8sVersion("prod"); err != nil || shared {
		t.Errorf("a pin that differs from spec.defaults is deployment-specific (shared=%v, err=%v)", shared, err)
	}
}

// THE EXEMPTION IS A PROOF, AND ONE BLIP MUST NOT BE WHAT FAILS TO PROVE IT. The
// caller refuses to let an unprovable exemption acquit a pin the catalog already
// disproved — which is right, and which makes a single-shot read dangerous in the
// other direction: one 503 on /lke/clusters would convict a long-lived deployment
// whose cluster is sitting there at the pinned version, blocking the apply it was
// running fine without.
func TestTheExemptionSurvivesOneTransientFailure(t *testing.T) {
	withSpec(t, specPinning("prod", "v1.33.6+lke7"), true, nil)
	withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
	calls := 0
	withClusters(t, func(context.Context) ([]map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("503 Service Unavailable")
		}
		return []map[string]any{lkeCluster("llz-prod", "us-ord", "v1.33.6+lke7")}, nil
	})
	if err := assertK8sVersion("prod"); err != nil {
		t.Fatalf("the second attempt proved the exemption; a blip on the first must not decide it: %v", err)
	}
	if calls != 2 {
		t.Errorf("made %d attempt(s), want 2 — the retry is what stops a transient error convicting "+
			"a running deployment", calls)
	}
}

// Bounded by ATTEMPTS, and it gives up rather than looping — after which the
// catalog verdict stands, because an exemption that could not be read is not
// evidence a cluster exists.
func TestTheExemptionGivesUpAndLeavesTheVerdictStanding(t *testing.T) {
	withSpec(t, specPinning("prod", "v1.33.6+lke7"), true, nil)
	withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
	calls := 0
	withClusters(t, func(context.Context) ([]map[string]any, error) {
		calls++
		return nil, errors.New("503 Service Unavailable")
	})
	if err := assertK8sVersion("prod"); err == nil {
		t.Fatal("the catalog disproved the pin and the exemption was never proved; the verdict stands")
	}
	if calls != linode.ClusterReadAttempts {
		t.Errorf("made %d attempt(s), want %d — bounded by attempts, not by a deadline",
			calls, linode.ClusterReadAttempts)
	}
}

// A PIN ONE CHARACTER OFF IS REJECTED, AND THE CHARACTER IS NAMED. The account
// offers v1.34.6+lke2; a spec pinning 1.34.6+lke2 is not in that list, and
// terraform sends the string verbatim. Printing the whole catalog and leaving the
// operator to spot the difference is the failure mode this arm exists to avoid.
func TestASpellingNearMissNamesTheCatalogEntry(t *testing.T) {
	_, err := k8sVersionVerdict("prod", "1.34.6+lke2", false, theE2EAccount, nil)
	if err == nil {
		t.Fatal("the pin is not in the catalog; terraform will send it as written")
	}
	for _, want := range []string{"1.34.6+lke2", "v1.34.6+lke2", "VERBATIM"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure should point at the one character to change; missing %q in:\n%s", want, err)
		}
	}
	// And it must not degrade into the generic list when there IS a near miss.
	if strings.Contains(err.Error(), "offered: ") {
		t.Errorf("a near miss is sharper than the catalog dump; got:\n%s", err)
	}
}

// OUR OWN DEADLINE IS NOT A TRANSIENT ERROR. Retrying against an expired context
// burns the pause and fails again for the same reason — and the caller then tells
// the operator to "re-run if that read was transient" about a budget that will
// expire identically next time.
func TestTheExemptionStopsRetryingOnceTheBudgetIsGone(t *testing.T) {
	calls, slept := 0, 0
	prev := k8sVersionSleep
	t.Cleanup(func() { k8sVersionSleep = prev })
	k8sVersionSleep = func(time.Duration) { slept++ }
	withClusters(t, func(context.Context) ([]map[string]any, error) {
		calls++
		return nil, errors.New("503 Service Unavailable")
	})

	// The parent budget having run out during the catalog read is the condition —
	// exercised directly, because the real probe timeout cannot be reached in a test
	// without spending it.
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := clustersForExemption(dead)
	if !errors.Is(err, errProbeBudget) {
		t.Fatalf("an expired parent budget must be reported as such, not as an API error: %v", err)
	}
	if calls != 1 {
		t.Errorf("made %d attempt(s), want 1 — an expired budget will expire again", calls)
	}
	if slept != 0 {
		t.Errorf("slept %d time(s) waiting to retry against a dead context", slept)
	}
}

// THE REGRESSION THE CLASSIFIER SHIPPED WITH, for exactly one commit. Go reports
// `http.Client.Timeout` as a WRAPPED context.DeadlineExceeded, so a classifier
// that asked the ERROR rather than the context read ONE SLOW REQUEST as the whole
// probe running out: it short-circuited this retry with most of the budget
// unspent, failed apply-vpc for a deployment whose cluster is running the pin, and
// told the operator a re-run was pointless.
func TestASlowRequestIsRetried(t *testing.T) {
	withSpec(t, specPinning("prod", "v1.33.6+lke7"), true, nil)
	withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
	calls := 0
	withClusters(t, func(context.Context) ([]map[string]any, error) {
		calls++
		if calls == 1 {
			// Byte-for-byte the shape net/http produces on a client timeout.
			return nil, fmt.Errorf("Get %q: %w (Client.Timeout exceeded while awaiting headers)",
				"https://api.linode.com/v4beta/lke/clusters", context.DeadlineExceeded)
		}
		return []map[string]any{lkeCluster("llz-prod", "us-ord", "v1.33.6+lke7")}, nil
	})
	if err := assertK8sVersion("prod"); err != nil {
		t.Fatalf("the second attempt proved the exemption; one slow request is exactly what the "+
			"retry exists for: %v", err)
	}
	if calls != 2 {
		t.Errorf("made %d attempt(s), want 2 — a per-request timeout is not the probe budget", calls)
	}
}

// A SLOW CATALOG READ MUST NOT STARVE THE EXEMPTION READ. The read budget used to
// size the parent deadline and nothing else, so overrunning one read ate the
// other's time — and the exemption read failing is the arm that convicts a
// deployment whose cluster is running the pin. Each read gets its own slice now,
// and this asserts the second one starts with a full one.
func TestEachReadGetsItsOwnSliceOfTheBudget(t *testing.T) {
	withSpec(t, specPinning("prod", "v1.33.6+lke7"), true, nil)
	withLister(t, func(ctx context.Context) ([]string, error) { return theE2EAccount, nil })
	var left time.Duration
	withClusters(t, func(ctx context.Context) ([]map[string]any, error) {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("the exemption read ran with no deadline")
		}
		left = time.Until(dl)
		return nil, nil
	})
	_ = assertK8sVersion("prod")
	// Its own slice, not whatever the catalog read left behind.
	if left > k8sVersionReadBudget || left < k8sVersionReadBudget-time.Second {
		t.Errorf("the exemption read started with %v, want ~%v — a read bounded by the parent "+
			"alone inherits whatever the previous one spent", left, k8sVersionReadBudget)
	}
}
