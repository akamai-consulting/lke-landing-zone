package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

type fakeLKELister struct {
	versions []string
	err      error
	tier     string // records what was asked for
	clusters []map[string]any
	clusterE error
	listed   bool // records whether the exemption was consulted at all

	clusterDeadline    time.Time
	hasClusterDeadline bool
	clusterCalls       int
	clusterOKAfter     int // succeed from this attempt onwards; 0 = always as configured
}

func (f *fakeLKELister) ListLKEVersions(_ context.Context, tier string) ([]string, error) {
	f.tier = tier
	return f.versions, f.err
}

func (f *fakeLKELister) ListClusters(ctx context.Context) ([]map[string]any, error) {
	f.listed = true
	f.clusterCalls++
	f.clusterDeadline, f.hasClusterDeadline = ctx.Deadline()
	if f.clusterOKAfter > 0 && f.clusterCalls >= f.clusterOKAfter {
		return f.clusters, nil
	}
	return f.clusters, f.clusterE
}

// pin is the common shape: one deployment, one version, a label+region that no
// fixture cluster matches unless the test says so.
func pin(v string) K8sPin {
	return K8sPin{Env: "prod", Version: v, ClusterLabel: "llz-prod", Region: "us-ord"}
}

// withLKELister installs a fake account; nil models "no LINODE_TOKEN".
func withLKELister(t *testing.T, l lkeVersionLister) {
	t.Helper()
	// The exemption retries with a real pause between attempts; no test should pay it.
	prevSleep := lkeSleep
	t.Cleanup(func() { lkeSleep = prevSleep })
	lkeSleep = func(time.Duration) {}
	prev := doctorLinodeClient
	t.Cleanup(func() { doctorLinodeClient = prev })
	doctorLinodeClient = func() lkeVersionLister {
		if l == nil {
			return nil
		}
		return l
	}
}

// willFailCI is the phrase a definite mismatch must print. It replaces the exit
// code this section briefly had: the complaint that made it fatal was never about
// doctor's STATUS, it was that a green doctor let an operator walk into a red
// build having been told they were ready.
const willFailCI = "will FAIL the build"

// UNCERTAINTY IS REPORTED, NEVER AS A VERDICT. Every shape below is a question
// that could not be answered — no token, an auth failure, an empty catalog, a
// catalog too coarse to settle the pin. None may claim the pin is bad.
func TestReportLinodeAccountNeverClaimsAVerdictItCouldNotReach(t *testing.T) {
	for name, l := range map[string]lkeVersionLister{
		"no token":            nil,
		"auth failure":        &fakeLKELister{err: errors.New("401 Invalid Token")},
		"empty catalog":       &fakeLKELister{versions: nil},
		"unparseable catalog": &fakeLKELister{err: errors.New("entry 1 carries no usable `id`")},
		"coarse catalog":      &fakeLKELister{versions: []string{"1.30", "1.31"}},
	} {
		withLKELister(t, l)
		out := captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
		if strings.Contains(out, willFailCI) {
			t.Errorf("%s: claimed CI will fail on a question it could not ask:\n%s", name, out)
		}
		if strings.Contains(out, "NOT in the account") {
			t.Errorf("%s: reported a verdict it could not reach:\n%s", name, out)
		}
	}
}

// THE OTHER HALF: a mismatch doctor CAN prove is one CI will fail on, and it has
// to say so in as many words. This section decides nothing — it reads whatever
// token is in the operator's shell, which need not be the account CI builds
// under — so volume is the whole mechanism.
func TestReportLinodeAccountSaysCIWillFailOnADefiniteMismatch(t *testing.T) {
	withLKELister(t, &fakeLKELister{versions: []string{"v1.34.6+lke2", "v1.32.9+lke4"}})
	out := captureStdout(t, func() {
		ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7"), pin("v1.34.6+lke2")})
	})
	if !strings.Contains(out, willFailCI) {
		t.Fatalf("a provable mismatch must warn that CI stops the build:\n%s", out)
	}
	// The offered pin must NOT be dressed up as a problem.
	if strings.Count(out, willFailCI) != 1 {
		t.Errorf("exactly one of the two pins is unbuildable:\n%s", out)
	}
	for _, want := range []string{"v1.33.6+lke7", "v1.34.6+lke2", "assert-k8s-version", "environments/prod.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report should mention %q so doctor and CI read as one instruction:\n%s", want, out)
		}
	}
}

func TestReportLinodeAccountWithoutAToken(t *testing.T) {
	withLKELister(t, nil)
	out := captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
	if !strings.Contains(out, "LINODE_TOKEN") {
		t.Errorf("should say how to enable the check, got:\n%s", out)
	}
}

// An auth failure is the interesting case — it is what a missing entitlement or
// an under-scoped PAT looks like — but it is also what a network blip looks like,
// so it reports as uncertainty, not as a verdict.
func TestReportLinodeAccountAuthFailureIsUncertainty(t *testing.T) {
	withLKELister(t, &fakeLKELister{err: errors.New("GET /v4beta/... returned 401 (check the PAT scope): Invalid Token")})
	out := captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
	for _, want := range []string{"could not list", "entitled"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestReportLinodeAccountFlagsAVersionTheAccountDoesNotOffer(t *testing.T) {
	withLKELister(t, &fakeLKELister{versions: []string{"v1.32.4+lke2", "v1.31.8+lke5"}})
	out := captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
	if !strings.Contains(out, "NOT in the account") {
		t.Errorf("a retired pin should be flagged:\n%s", out)
	}
	if !strings.Contains(out, "v1.32.4+lke2") {
		t.Errorf("should list what IS offered:\n%s", out)
	}
}

func TestReportLinodeAccountAsksTheEnterpriseTier(t *testing.T) {
	f := &fakeLKELister{versions: []string{"v1.33.6+lke7"}}
	withLKELister(t, f)
	captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
	if f.tier != "enterprise" {
		t.Errorf("tier = %q, want enterprise — the standard catalog answers for a product LLZ does not use", f.tier)
	}
}

// `k8s_version` reaches the API only on a create or a change, so a cluster
// already running the pin makes the apply a no-op whatever the catalog says
// today. Without this, the first rotation (measured happening inside an hour)
// would make doctor shout about every healthy long-lived deployment.
func TestReportLinodeAccountExemptsAClusterAlreadyRunningThePin(t *testing.T) {
	withLKELister(t, &fakeLKELister{
		versions: []string{"v1.34.6+lke2", "v1.32.9+lke4"},
		clusters: []map[string]any{{"label": "llz-prod", "region": "us-ord", "k8s_version": "v1.33.6+lke7"}},
	})
	out := captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
	if strings.Contains(out, willFailCI) {
		t.Fatalf("the cluster already runs the pin; nothing is blocked:\n%s", out)
	}
	// It must still SAY so — silence would hide that the deployment can no longer
	// be re-created, which someone otherwise finds out during a rebuild.
	if !strings.Contains(out, "already runs it") {
		t.Errorf("the exemption should be reported, not silent:\n%s", out)
	}
}

func TestReportLinodeAccountStillWarnsWhenNothingRunsThePin(t *testing.T) {
	withLKELister(t, &fakeLKELister{
		versions: []string{"v1.34.6+lke2"},
		clusters: []map[string]any{{"label": "llz-lab", "region": "us-ord", "k8s_version": "v1.33.6+lke7"}},
	})
	out := captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
	if !strings.Contains(out, willFailCI) {
		t.Fatalf("a different deployment running the pin is not this one's cluster:\n%s", out)
	}
}

// AN UNREAD EXEMPTION IS NOT A PREDICTION EITHER WAY. The pin is not in the
// catalog, but whether a cluster already runs it — which exempts the deployment
// entirely — could not be read HERE, and CI makes that read itself with its own
// retries. Asserting "CI will fail this" off a local 503 sends an operator to bump
// cluster.k8sVersion for a deployment that never needed it; saying nothing hides a
// pin that probably is stale. It reports both halves and predicts neither.
func TestReportLinodeAccountDoesNotPredictCIOnAnUnreadExemption(t *testing.T) {
	withLKELister(t, &fakeLKELister{versions: []string{"v1.34.6+lke2"}, clusterE: errors.New("503 Service Unavailable")})
	out := captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
	if strings.Contains(out, willFailCI) {
		t.Errorf("CI re-reads the cluster list and may exempt this deployment; a local 503 "+
			"cannot say the build fails:\n%s", out)
	}
	for _, want := range []string{"could not be checked here", "v1.34.6+lke2", "assert-k8s-version"} {
		if !strings.Contains(out, want) {
			t.Errorf("it must still report what it DID find (%q missing):\n%s", want, out)
		}
	}
}

// The exemption is a last question before reporting, so an all-offered account
// costs one request rather than two.
func TestReportLinodeAccountDoesNotListClustersWhenEveryPinIsOffered(t *testing.T) {
	f := &fakeLKELister{versions: []string{"v1.33.6+lke7"}}
	withLKELister(t, f)
	captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
	if f.listed {
		t.Error("listed the account's clusters though every pin was plainly offered")
	}
}

// The probe budget must cover EVERY read it allows. It used to equal the
// per-request timeout, because the section made one request; the exemption added
// a second, so a slow catalog read would leave the cluster read to be cancelled by
// the parent.
//
// Arithmetic, not a watched deadline: the stubbed catalog read returns instantly,
// so a check of "how much was left at the second call" passes at the buggy value
// too — a test that cannot fail on the regression it names.
func TestTheProbeBudgetCoversEveryReadItAllows(t *testing.T) {
	worst := (1+linode.ClusterReadAttempts)*lkeReadBudget +
		linode.ClusterReadAttempts*linode.ClusterReadRetryPause
	if lkeProbeTimeout < worst {
		t.Errorf("probe budget %v cannot cover one catalog read plus %d exemption attempt(s) at %v "+
			"each with %v pauses (%v)", lkeProbeTimeout, linode.ClusterReadAttempts,
			lkeReadBudget, linode.ClusterReadRetryPause, worst)
	}
}

// Separately: the reads must be bounded at all, or doctor hangs on a wedged API.
func TestTheClusterReadIsBounded(t *testing.T) {
	f := &fakeLKELister{versions: []string{"v1.34.6+lke2"}}
	withLKELister(t, f)
	captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
	if !f.hasClusterDeadline {
		t.Fatal("the cluster read ran with no deadline — doctor would hang on a wedged API")
	}
}

// A COARSE CATALOG CANNOT SPEAK AT BUILD PRECISION IN EITHER DIRECTION, and the
// agreement case is the one that used to print "is offered" — an unqualified pass
// for a retired build.
func TestReportLinodeAccountOnACoarseCatalog(t *testing.T) {
	for _, versions := range [][]string{{"1.33", "1.32"}, {"1.30", "1.31"}} {
		f := &fakeLKELister{versions: versions}
		withLKELister(t, f)
		out := captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
		if strings.Contains(out, willFailCI) {
			t.Errorf("%v has no standing to reject a +lke build:\n%s", versions, out)
		}
		if !strings.Contains(out, "UNCHECKED") {
			t.Errorf("%v cannot confirm one either; uncertainty must be reported rather than "+
				"passed as a verdict:\n%s", versions, out)
		}
		if f.listed {
			t.Error("read the cluster list for a pin that was never disproved — the exemption is " +
				"an escape from a definite negative, and there is none here")
		}
	}
}

// The remediation must name the file the pin is actually in — landingzone.yaml.example
// seeds it in spec.defaults, where every deployment inherits it, so naming the
// per-deployment file unblocks ONE and leaves the rest failing one at a time.
func TestDoctorNamesWhereThePinActuallyLives(t *testing.T) {
	withLKELister(t, &fakeLKELister{versions: []string{"v1.34.6+lke2"}})
	out := captureStdout(t, func() {
		ReportLinodeAccount([]K8sPin{{Env: "prod", Version: "v1.33.6+lke7", ClusterLabel: "llz-prod", Region: "us-ord", Shared: true}})
	})
	for _, want := range []string{"landingzone.yaml", "spec.defaults", "environments/prod.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("an inherited pin must name %q:\n%s", want, out)
		}
	}

	withLKELister(t, &fakeLKELister{versions: []string{"v1.34.6+lke2"}})
	out = captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
	if strings.Contains(out, "landingzone.yaml") {
		t.Errorf("a deployment-specific pin is fixed in its own file:\n%s", out)
	}
}

// One blip on /lke/clusters must not make doctor shout about a deployment whose
// cluster is sitting right there at the pinned version. Mirrors the CI gate's arm.
func TestTheExemptionSurvivesOneTransientFailure(t *testing.T) {
	f := &fakeLKELister{
		versions:       []string{"v1.34.6+lke2"},
		clusters:       []map[string]any{{"label": "llz-prod", "region": "us-ord", "k8s_version": "v1.33.6+lke7"}},
		clusterE:       errors.New("503 Service Unavailable"),
		clusterOKAfter: 2,
	}
	withLKELister(t, f)
	out := captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
	if strings.Contains(out, willFailCI) {
		t.Fatalf("the second attempt proved the exemption:\n%s", out)
	}
	if f.clusterCalls != 2 {
		t.Errorf("made %d attempt(s), want 2", f.clusterCalls)
	}
}

// REACHABLE IS NOT CHECKED, and nothing to check means no request either — the
// sibling CI verb states that rule and follows it.
func TestReportLinodeAccountDoesNotImplyItCheckedAnythingWithNoPins(t *testing.T) {
	f := &fakeLKELister{versions: []string{"v1.34.6+lke2"}}
	withLKELister(t, f)
	out := captureStdout(t, func() { ReportLinodeAccount(nil) })
	if !strings.Contains(out, "no k8sVersion pins to check") {
		t.Errorf("a section that examined nothing must say so:\n%s", out)
	}
	if strings.Contains(out, "reachable") {
		t.Errorf("a green reachability line reads as a clean bill of health for a deployment "+
			"nobody looked at:\n%s", out)
	}
	if f.tier != "" {
		t.Error("asked the Linode API with no pins to check")
	}
}

// Availability is per-ACCOUNT and this reads the operator's shell credential,
// which need not be the account CI builds under — which is exactly why it reports
// rather than decides, and why the report has to say whose answer it is.
func TestReportLinodeAccountNamesTheCredentialThatAnswered(t *testing.T) {
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("LINODE_API_TOKEN", "tok")
	withLKELister(t, &fakeLKELister{versions: []string{"v1.34.6+lke2"}})
	out := captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })
	for _, want := range []string{"LINODE_API_TOKEN", "may be a different account"} {
		if !strings.Contains(out, want) {
			t.Errorf("a verdict from a possibly-different account must say so; missing %q in:\n%s", want, out)
		}
	}
}

// Doctor names the near miss too — the two reports are one instruction, and an
// operator who reads doctor should not get a vaguer answer than CI's.
func TestDoctorNamesASpellingNearMiss(t *testing.T) {
	withLKELister(t, &fakeLKELister{versions: []string{"v1.34.6+lke2", "v1.32.9+lke4"}})
	out := captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("1.34.6+lke2")}) })
	if !strings.Contains(out, willFailCI) {
		t.Fatalf("the pin is not in the catalog and terraform sends it verbatim:\n%s", out)
	}
	if !strings.Contains(out, "v1.34.6+lke2") || !strings.Contains(out, "spelled differently") {
		t.Errorf("should point at the one character to change:\n%s", out)
	}
}
