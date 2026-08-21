package tokeninv

// linode_capability_test.go — the CONSUMER half of the route contract, and the
// gate for the discrimination this check exists to make (issue #449).
//
// `llz ci assert-k8s-version` warns and PASSES when the LKE-Enterprise version
// catalog refuses it. That is the right rule — a build must not be blocked on a
// question nobody could ask — and it means the gate can be permanently inert in
// exactly the pipeline the incident happened in, with nothing anywhere saying so.
// This check is what says so, and it can only do that if it separates two 401s
// that look identical from a single probe:
//
//	the PAT lacks the LKE grant   → blocking. The cluster apply fails on it anyway,
//	                                ~15 minutes later.
//	the PAT has it, route refuses → not the token. Report the GATE as inert.
//
// Getting that backwards in either direction is worse than not probing: one blocks
// builds on a correctly-scoped credential, the other files a measured, reproducible
// refusal under "could not verify" and loses it.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// withLinodeRoutes stubs the Linode capability probe with a per-path answer, so a
// test states exactly which doors are open. An unlisted path fails the test rather
// than defaulting — a probe hitting a route no case anticipated is a bug the
// fixture must not absorb.
func withLinodeRoutes(t *testing.T, codes map[string]int) *[]string {
	t.Helper()
	orig := LinodeCapabilityProbe
	t.Cleanup(func() { LinodeCapabilityProbe = orig })
	var asked []string
	LinodeCapabilityProbe = func(_, _, path string) (int, error) {
		asked = append(asked, path)
		code, ok := codes[path]
		if !ok {
			t.Errorf("probed an unexpected route %q", path)
			return 0, nil
		}
		return code, nil
	}
	return &asked
}

// TestTheCatalogProbeAsksTheRouteTheGateActuallyReads is the consumer half of the
// contract linode.TestTheReadsUseTheExportedRouteDefinitions holds up on the
// producer side. Between them, a route change that does not move the exported
// definition is red on one side and a probe that spells its own copy of the route
// is red on this one.
func TestTheCatalogProbeAsksTheRouteTheGateActuallyReads(t *testing.T) {
	want := linode.LKEVersionsPath(linode.LKETierEnterprise)
	asked := withLinodeRoutes(t, map[string]int{want: 200})

	cr := probeCapability(capCheckFor(t, "LINODE_API_TOKEN"), "tok")
	if cr.status != capOK {
		t.Fatalf("200 → status %v, want capOK", cr.status)
	}
	if len(*asked) != 1 || (*asked)[0] != want {
		t.Errorf("probed %v, want exactly [%s] — the second opinion costs a request and must be "+
			"asked ONLY on a refusal", *asked, want)
	}
}

// TestARefusalAtBothRoutesIsTheMissingGrant — the blocking arm. Both routes need
// `lke:read_only`; refused at both, the credential simply does not have it, and
// the cluster apply would die on that ~15 minutes in.
func TestARefusalAtBothRoutesIsTheMissingGrant(t *testing.T) {
	withLinodeRoutes(t, map[string]int{
		linode.LKEVersionsPath(linode.LKETierEnterprise): 401,
		linode.LKEClustersPath:                           401,
	})

	cr := probeCapability(capCheckFor(t, "LINODE_API_TOKEN"), "tok")
	if cr.status != capDenied {
		t.Fatalf("refused at both LKE routes → status %v, want capDenied (this blocks)", cr.status)
	}
	// It must rule the ROUTE out — that is what the second probe buys — WITHOUT
	// asserting the scope explanation it cannot separate from an account-level one.
	// Two routes sharing a grant also share the API version and the account, so a
	// cause up there refuses both; the verdict is right either way (all of them stop
	// the cluster apply) but the message must not send an operator to re-scope a PAT
	// that already carries the grant.
	if !strings.Contains(cr.detail, "not one fussy route") {
		t.Errorf("the verdict must say the route has been ruled out; got %q", cr.detail)
	}
	if !strings.Contains(cr.detail, "account-level") {
		t.Errorf("the verdict must name the candidate it cannot rule out, or a correctly scoped PAT "+
			"costs an afternoon; got %q", cr.detail)
	}
	// Re-scope, never rotate: the validity probe already passed, so the token is live
	// and minting a replacement with the same gap costs an afternoon.
	h := capabilityHint("LINODE_API_TOKEN")
	if !strings.Contains(h, "Read Only") {
		t.Errorf("the remediation must name the grant to add; got %q", h)
	}
}

// TestAScopedTokenRefusedAtOneRouteReportsTheGateInert — the arm this check was
// built for. The credential is fine. What is not fine is that a gate downstream
// has been PROVEN unanswerable in this pipeline, and that finding used to exist
// only as a warning inside that gate's own green step.
func TestAScopedTokenRefusedAtOneRouteReportsTheGateInert(t *testing.T) {
	withLinodeRoutes(t, map[string]int{
		linode.LKEVersionsPath(linode.LKETierEnterprise): 401,
		linode.LKEClustersPath:                           200,
	})

	cr := probeCapability(capCheckFor(t, "LINODE_API_TOKEN"), "tok")
	if cr.status != capRouteRefused {
		t.Fatalf("scoped-but-refused → status %v, want capRouteRefused — capDenied would block a "+
			"build on a correctly scoped PAT, capUnknown would lose a reproducible finding", cr.status)
	}
	// The consequence is the payload. A verdict about a credential that the reader
	// has to connect to a gate three steps later is the silence all over again.
	for _, want := range []string{"assert-k8s-version", "INERT"} {
		if !strings.Contains(cr.detail, want) {
			t.Errorf("the verdict must name %q — what is broken is the CHECK, not the token; got %q", want, cr.detail)
		}
	}
	// Asserted as a POSITIVE, not as the absence of the word "re-scope". The
	// verdict is arrived at by overriding a refusal whose own rendered advice IS
	// "re-scope it", and the first cut pasted that sentence in wholesale — so the
	// thing worth pinning is that this arm states the opposite out loud rather than
	// merely omitting it.
	if !strings.Contains(cr.detail, "Nothing to re-scope") {
		t.Errorf("this token is correctly scoped, and the arm it overrides says otherwise — it must "+
			"say so explicitly or an operator reads the refusal and goes to mint a PAT: %q", cr.detail)
	}
}

// TestAnUncorroboratedRefusalIsUnresolvedRatherThanEither — the second opinion
// could not be taken, so neither conclusion is available. Blocking here would fail
// a build on a blip at a route nothing was asking about; calling it inert would
// claim a measurement that was not made.
func TestAnUncorroboratedRefusalIsUnresolvedRatherThanEither(t *testing.T) {
	withLinodeRoutes(t, map[string]int{
		linode.LKEVersionsPath(linode.LKETierEnterprise): 401,
		linode.LKEClustersPath:                           500,
	})

	cr := probeCapability(capCheckFor(t, "LINODE_API_TOKEN"), "tok")
	if cr.status != capUnknown {
		t.Fatalf("uncorroborated refusal → status %v, want capUnknown", cr.status)
	}
	if !strings.Contains(cr.detail, "UNRESOLVED") {
		t.Errorf("the verdict must say the question was not settled; got %q", cr.detail)
	}
}

// TestTheLinodeRefusalAdviceDoesNotOfferGitHubCauses — a 401 at a Linode route
// has nothing to do with SAML SSO, and offering it as a cause sends an operator to
// a setting that does not exist on the platform they are on.
func TestTheLinodeRefusalAdviceDoesNotOfferGitHubCauses(t *testing.T) {
	_, detail := classifyCapabilityStatus(401, "read the catalog", capLinode)
	if strings.Contains(detail, "SSO") {
		t.Errorf("Linode PATs have no SSO authorization step; got %q", detail)
	}
	if _, gh := classifyCapabilityStatus(401, "fetch the repo", capGit); !strings.Contains(gh, "SSO") {
		t.Errorf("the git door's 401 must still offer SSO — it is the cause that check was built "+
			"for; got %q", gh)
	}
}

// TestEachDoorKeepsTheRemedyForEveryCauseItNames — the regression this file's
// transport split caused, and the reason the cause and the remedy are now both
// transport-aware. Making only the CAUSE vary dropped "SSO-authorize it" from the
// GitHub 401 while that same line still offered SSO as a cause, so a correctly
// scoped APL_VALUES_REPO_TOKEN that was simply not SSO-authorized blocked the run
// under advice that could not fix it. The existing test checked the cause only.
func TestEachDoorKeepsTheRemedyForEveryCauseItNames(t *testing.T) {
	_, gh := classifyCapabilityStatus(401, "fetch the repo", capGit)
	if !strings.Contains(gh, "SSO-authorize") {
		t.Errorf("the git door names SSO as a cause, so it must name SSO authorization as a "+
			"remedy; got %q", gh)
	}
	_, lin := classifyCapabilityStatus(401, "read the catalog", capLinode)
	if strings.Contains(lin, "SSO-authorize") {
		t.Errorf("a Linode PAT has no SSO authorization step to perform; got %q", lin)
	}
	for _, d := range []string{gh, lin} {
		if !strings.Contains(d, "don't rotate it") {
			t.Errorf("capability is only asked of a token that already authenticated, so no door may "+
				"prescribe rotation; got %q", d)
		}
	}
}

// TestASecondOpinionIsOnlyRegisteredWhereTheRoutesShareAGrant. A second route
// needing a DIFFERENT permission would acquit an under-scoped token — the failure
// this whole file exists to prevent, reached from the other side.
func TestASecondOpinionIsOnlyRegisteredWhereTheRoutesShareAGrant(t *testing.T) {
	for _, c := range capabilityChecks {
		if c.secondOpinion == nil {
			continue
		}
		if c.token != "LINODE_API_TOKEN" {
			t.Errorf("%s registers a second opinion; confirm both routes need the SAME grant and "+
				"add it here deliberately", c.token)
			continue
		}
		p, _ := c.secondOpinion()
		if p != linode.LKEClustersPath {
			t.Errorf("second opinion probes %q, want the LKE cluster list — it is the route that "+
				"shares `lke:read_only` with the catalog", p)
		}
	}
}

// TestTheLinodeProbeSendsABearerToken exercises the REAL probe body — the one
// thing the seam above can never check, and the one most likely to be wrong.
//
// The two GitHub probes in this file authenticate as `Authorization: token <pat>`
// and `Basic`; Linode wants `Bearer`. Copying either neighbour's header would
// produce a probe that 401s against every account, which this check would then
// report as a refused route — a permanent, plausible-looking "the gate is inert"
// verdict caused entirely by the probe. It is worth one httptest server.
func TestTheLinodeProbeSendsABearerToken(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	code, err := LinodeCapabilityProbe(srv.URL, "tok", linode.LKEVersionsPath(linode.LKETierEnterprise))
	if err != nil || code != 200 {
		t.Fatalf("probe = (%d, %v), want (200, nil)", code, err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want %q — Linode rejects the `token <pat>` scheme its "+
			"GitHub neighbours in this file use", gotAuth, "Bearer tok")
	}
	if want := linode.LKEVersionsPath(linode.LKETierEnterprise); gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

// TestTheLinodeProbeReportsUnreachableAsZero — an unreachable host must come back
// as code 0 so classifyCapabilityStatus files it under "could not verify" rather
// than under a refusal. A probe that surfaced a dial error as a denial would block
// builds on connectivity.
func TestTheLinodeProbeReportsUnreachableAsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	code, err := LinodeCapabilityProbe(url, "tok", "/v4beta/lke/tiers/enterprise/versions")
	if err == nil {
		t.Fatal("a dead endpoint must return an error for the caller to map to code 0")
	}
	if code != 0 {
		t.Errorf("code = %d, want 0 on an unreachable endpoint", code)
	}
}

// TestTheInertVerdictRendersAsItsOwnThing — capRouteRefused must not fall through
// to the default cell, which reads "– not probed": the probe ran, twice, and
// reached a conclusion.
func TestTheInertVerdictRendersAsItsOwnThing(t *testing.T) {
	cell := capabilityCell(capabilityResult{"LINODE_API_TOKEN", capRouteRefused, "the detail"})
	if !strings.Contains(cell, "INERT") || !strings.Contains(cell, "the detail") {
		t.Errorf("the cell must name the state and carry the detail; got %q", cell)
	}
}
