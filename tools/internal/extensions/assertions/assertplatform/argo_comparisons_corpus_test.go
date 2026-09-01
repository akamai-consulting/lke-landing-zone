package assertplatform

// argo_comparisons_corpus_test.go — what the sweep says about the REAL
// Application from the outage, which is: nothing.
//
// THE POINT IS THE NEGATIVE. The write-up this gate was built from inferred that
// a failed server-side diff would surface as a ComparisonError, and the sweep was
// written expecting to be the detector. The live Application says otherwise:
// `sync: Synced`, `health: Progressing`, an operationState that SUCCEEDED twenty
// days before the desired state it claims to have synced — and no conditions at
// all. Argo records nothing.
//
// So this fixture pins the limit of what a clean sweep means. A future change
// that made this test start FAILING — the sweep reporting a finding here — would
// be reporting one Argo does not provide, and the reason it passes must never be
// paraphrased as "the estate is fine".

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
)

func liveApplication(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading the captured Application: %v", err)
	}
	return json.RawMessage(raw)
}

func TestTheRealUndeliveredApplicationCarriesNoConditionAtAll(t *testing.T) {
	raw := liveApplication(t, "monitoring-loki.synced-no-conditions.json")
	app, err := health.ParseArgoApp(raw)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if app.Sync != "Synced" {
		t.Errorf("sync = %q, want Synced — this fixture is the silent-green case", app.Sync)
	}
	if app.SpecErr != "" {
		t.Fatalf("the captured Application carries a spec error (%q); it was captured precisely because it "+
			"carries NONE, and the whole argument for assert-overlay-applied rests on that", app.SpecErr)
	}
	if got := comparisonFindings([]health.ArgoApp{app}); len(got) != 0 {
		t.Errorf("the sweep reported %d finding(s) on an Application Argo records nothing about: %+v. "+
			"A finding here would be invented", len(got), got)
	}
}

// …and the whole lane passes on it, which is the honest and dangerous half: this
// is what a green `assert-argo-comparisons` looks like on a cluster carrying an
// undelivered change. The runbook says a clean sweep rules a cause IN, never out;
// this is the evidence for that sentence.
func TestTheLanePassesOnTheClusterThatWasBrokenForWeeks(t *testing.T) {
	withApplications(t, []json.RawMessage{liveApplication(t, "monitoring-loki.synced-no-conditions.json")}, true)
	if err := assertArgoComparisons("argocd"); err != nil {
		t.Fatalf("assert-argo-comparisons = %v, want nil — Argo recorded no condition, so there is nothing "+
			"for this lane to find. If this ever fails, the sweep has started inventing findings", err)
	}
}

// The captured Application is platform-owned, so a finding on it WOULD have
// gated. That matters: the sweep passing here is about Argo recording nothing,
// not about the app being out of scope.
func TestTheCapturedApplicationIsPlatformScoped(t *testing.T) {
	app, err := health.ParseArgoApp(liveApplication(t, "monitoring-loki.synced-no-conditions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if health.IsInstanceOwnedApp(app) {
		t.Errorf("the captured Application reads as instance-owned (project %q) — then its silence would "+
			"prove nothing about the platform gate", app.Project)
	}
}
