package assertplatform

// argo_comparisons_test.go covers the sweep's pure evaluator, its fail-closed
// arms, and — the part that matters — the COUPLING to health.ParseArgoApp.
//
// The coupling test feeds a real Application document through the real parser
// into the real evaluator. Restating "a ComparisonError condition means SpecErr"
// in a fixture would pass forever while the parser stopped populating the field,
// which is the shape this repo's two live regressions both had.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
)

// withApplications swaps the transport seam for the duration of a test.
func withApplications(t *testing.T, items []json.RawMessage, answered bool) {
	t.Helper()
	prev := readApplications
	readApplications = func(string) ([]json.RawMessage, bool) { return items, answered }
	t.Cleanup(func() { readApplications = prev })
}

// appDoc builds one Application document. Written as JSON text rather than as a
// struct so the test exercises the same decode path the cluster feeds.
func appDoc(name, project, sync, healthStatus string, conditions string) json.RawMessage {
	return json.RawMessage(`{
      "metadata": {"name": "` + name + `"},
      "spec": {"project": "` + project + `", "syncPolicy": {"automated": {}}},
      "status": {
        "sync": {"status": "` + sync + `"},
        "health": {"status": "` + healthStatus + `"},
        "conditions": [` + conditions + `]
      }}`)
}

const comparisonErrCondition = `{"type": "ComparisonError", "message": "failed to generate manifest: boom"}`

func TestComparisonFindingsNamesOnlyAppsThatFailedToCompare(t *testing.T) {
	apps := []health.ArgoApp{
		{Name: "clean", Sync: "Synced", Health: "Healthy"},
		{Name: "broken", Sync: "Unknown", Health: "Degraded", SpecErr: "ComparisonError: boom"},
	}
	got := comparisonFindings(apps)
	if len(got) != 1 || got[0].App != "broken" {
		t.Fatalf("expected only the app with a spec error, got %+v", got)
	}
	if !got[0].Gating {
		t.Error("a platform Application's failed comparison must gate")
	}
	if got[0].Silent {
		t.Error("Silent marks a comparison error hiding behind a Synced status; this one is Unknown")
	}
}

// THE SHAPE THE SWEEP EXISTS FOR. Synced plus a comparison error is not two
// facts in tension — it is one stale fact and one current one, and the report has
// to say which is which.
func TestASyncedApplicationWithAComparisonErrorIsMarkedSilent(t *testing.T) {
	got := comparisonFindings([]health.ArgoApp{
		{Name: "monitoring-loki", Sync: "Synced", Health: "Healthy", SpecErr: "ComparisonError: boom"},
	})
	if len(got) != 1 || !got[0].Silent {
		t.Fatalf("a Synced app carrying a comparison error must be marked Silent, got %+v", got)
	}
	line := renderComparisonFinding(got[0])
	for _, want := range []string{"sync.status still reads Synced", "selfHeal never fires"} {
		if !strings.Contains(line, want) {
			t.Errorf("the silent-green line must explain itself; %q missing from:\n%s", want, line)
		}
	}
}

// The boundary is health's, not this file's — an instance-owned app's failure is
// printed and does not gate the platform.
func TestAnInstanceOwnedApplicationIsReportedButDoesNotGate(t *testing.T) {
	got := comparisonFindings([]health.ArgoApp{
		{Name: "dispatch", Project: health.InstanceCustomProject, Sync: "Synced", SpecErr: "ComparisonError: boom"},
	})
	if len(got) != 1 {
		t.Fatalf("an instance-owned failure must still be REPORTED, got %+v", got)
	}
	if got[0].Gating {
		t.Error("an instance-owned Application must not gate the platform")
	}
	if !strings.Contains(renderComparisonFinding(got[0]), "instance-owned") {
		t.Error("the line must say why it is not gating")
	}
}

// ── the coupling: the parser really does populate what the evaluator reads ────

func TestTheEvaluatorReadsWhatParseArgoAppActuallyPopulates(t *testing.T) {
	a, err := health.ParseArgoApp(appDoc("monitoring-loki", "default", "Synced", "Healthy", comparisonErrCondition))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	got := comparisonFindings([]health.ArgoApp{a})
	if len(got) != 1 {
		t.Fatalf("a document carrying a ComparisonError condition must produce a finding; "+
			"ParseArgoApp gave SpecErr=%q", a.SpecErr)
	}
	if !strings.Contains(got[0].Err, "failed to generate manifest") {
		t.Errorf("the API server's own message must survive to the report, got %q", got[0].Err)
	}
}

// ── fail-closed arms ─────────────────────────────────────────────────────────

func TestAnUnreadableApiserverFailsRatherThanPasses(t *testing.T) {
	withApplications(t, nil, false)
	if err := assertArgoComparisons("argocd"); err == nil {
		t.Fatal("could-not-read must fail: 'could not tell' is not 'nothing wrong'")
	}
}

func TestNoApplicationsAtAllFailsRatherThanPasses(t *testing.T) {
	withApplications(t, nil, true)
	err := assertArgoComparisons("argocd")
	if err == nil {
		t.Fatal("a namespace with no Applications means nothing was examined — that must not pass")
	}
	if !strings.Contains(err.Error(), "examined nothing") {
		t.Errorf("the vacuity failure must say so, got %v", err)
	}
}

func TestAMalformedApplicationFailsRatherThanBeingSkipped(t *testing.T) {
	withApplications(t, []json.RawMessage{json.RawMessage(`{"metadata":`)}, true)
	if err := assertArgoComparisons("argocd"); err == nil {
		t.Fatal("an Application that cannot be parsed is a finding, not something to skip past")
	}
}

func TestACleanEstatePasses(t *testing.T) {
	withApplications(t, []json.RawMessage{
		appDoc("platform-bootstrap", "default", "Synced", "Healthy", ""),
		appDoc("monitoring-loki", "default", "Synced", "Healthy", ""),
	}, true)
	if err := assertArgoComparisons("argocd"); err != nil {
		t.Fatalf("two cleanly-compared Applications must pass, got %v", err)
	}
}

func TestAPlatformComparisonErrorFailsTheLane(t *testing.T) {
	withApplications(t, []json.RawMessage{
		appDoc("platform-bootstrap", "default", "Synced", "Healthy", ""),
		appDoc("monitoring-loki", "default", "Synced", "Healthy", comparisonErrCondition),
	}, true)
	if err := assertArgoComparisons("argocd"); err == nil {
		t.Fatal("a platform Application that could not be compared must fail the lane")
	}
}

func TestOnlyInstanceOwnedComparisonErrorsDoNotFailTheLane(t *testing.T) {
	withApplications(t, []json.RawMessage{
		appDoc("platform-bootstrap", "default", "Synced", "Healthy", ""),
		appDoc("dispatch", health.InstanceCustomProject, "Synced", "Healthy", comparisonErrCondition),
	}, true)
	if err := assertArgoComparisons("argocd"); err != nil {
		t.Fatalf("an instance-owned failure is reported, not gated, got %v", err)
	}
}

// THE SWEEP MUST TOLERATE WHAT converge TOLERATES, or the gating lane it runs in
// is permanently red on healthy instances — which is how a gate gets switched
// off. classifyArgoApp demotes an operator-deferred app and a Redis cache auth
// split before it looks at SpecErr; this pins that the sweep does the same.
func TestTheSweepAppliesConvergesOwnDemotions(t *testing.T) {
	deferredApps := health.ExternalDepApps()
	if len(deferredApps) == 0 {
		t.Skip("no operator-deferred apps declared")
	}
	deferredName := deferredApps[0].Pattern

	got := comparisonFindings([]health.ArgoApp{
		{Name: deferredName, Sync: "Unknown", SpecErr: "ComparisonError: no DNS token"},
		{Name: "any-app", Sync: "Unknown", SpecErr: "ComparisonError: failed to list refs: WRONGPASS"},
		{Name: "real-fault", Sync: "Synced", SpecErr: "ComparisonError: failed to generate manifest"},
	})
	if len(got) != 3 {
		t.Fatalf("all three must be REPORTED; only the gating verdict differs. got %d", len(got))
	}
	byApp := map[string]comparisonFinding{}
	for _, f := range got {
		byApp[f.App] = f
	}
	if byApp[deferredName].Gating {
		t.Errorf("%s is operator-deferred and sits in a permanent ComparisonError on any instance without "+
			"its credential — gating on it makes this lane impossible to turn green", deferredName)
	}
	if byApp[deferredName].Tolerated == "" {
		t.Error("a tolerated finding must say why it is tolerated")
	}
	if byApp["any-app"].Gating {
		t.Error("a Redis cache auth split is transient and converge repairs it; it must not gate")
	}
	if !byApp["real-fault"].Gating {
		t.Error("a genuine manifest fault must still gate — the demotions are exceptions, not a policy")
	}
}

// A git-auth refusal is NOT demoted, matching classifyArgoApp: the remote
// answered, the answer was no, and polling cannot change it.
func TestAGitAuthRefusalStillGates(t *testing.T) {
	got := comparisonFindings([]health.ArgoApp{
		{Name: "gitops-global", Sync: "Unknown", SpecErr: "ComparisonError: authentication required"},
	})
	if len(got) != 1 || !got[0].Gating {
		t.Errorf("a credential the remote refuses is terminal and must gate, got %+v", got)
	}
}

// classifyArgoApp checks the 256KB annotation wedge BEFORE it reads SpecErr — it
// is an infra fault converge repairs by stripping the annotation. An app carrying
// both that and a ComparisonError is pending to converge, and must not be red
// here.
func TestAnAnnotationLimitWedgeIsToleratedLikeConvergeTreatsIt(t *testing.T) {
	got := comparisonFindings([]health.ArgoApp{{
		Name:    "platform-bootstrap",
		Sync:    "OutOfSync",
		SpecErr: "ComparisonError: failed to generate manifest",
		OpErr:   `CustomResourceDefinition "policies.kyverno.io" is invalid: metadata.annotations: Too long`,
	}})
	if len(got) != 1 {
		t.Fatalf("it must still be reported, got %d", len(got))
	}
	if got[0].Gating {
		t.Error("converge grades this pending and strips the annotation itself; gating on it makes the " +
			"lane red over a fault the platform repairs")
	}
	if got[0].Tolerated == "" {
		t.Error("a tolerated finding must say why")
	}
}

// "None of them is platform-owned" was printed for every non-gating reason,
// including a Redis auth split — which is the whole platform estate failing to
// compare at once. Reading that as somebody else's content is the opposite of
// what happened.
func TestAToleratedEstateDoesNotReadAsInstanceOwned(t *testing.T) {
	withApplications(t, []json.RawMessage{
		appDoc("platform-bootstrap", "default", "Unknown", "Healthy",
			`{"type":"ComparisonError","message":"failed to list refs: WRONGPASS invalid username-password pair"}`),
		appDoc("monitoring-loki", "default", "Unknown", "Healthy",
			`{"type":"ComparisonError","message":"failed to list refs: WRONGPASS invalid username-password pair"}`),
	}, true)
	out := captureSweep(t, func() error { return assertArgoComparisons("argocd") })
	if strings.Contains(out, "None of them is platform-owned") {
		t.Errorf("a redis auth split across the platform estate must not read as instance-owned "+
			"content:\n%s", out)
	}
	if !strings.Contains(out, "converge tolerates") {
		t.Errorf("the pass must say WHY it does not gate:\n%s", out)
	}
}

// captureSweep runs fn with stdout captured and fails the test if it errored —
// these cases are all supposed to pass the lane. Local to this file so the sweep
// and its tests do not depend on the other verb's helpers.
func captureSweep(t *testing.T, fn func() error) string {
	t.Helper()
	prev := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	runErr := fn()
	_ = w.Close()
	os.Stdout = prev
	out := <-done
	if runErr != nil {
		t.Fatalf("assertArgoComparisons = %v, want nil", runErr)
	}
	return out
}
