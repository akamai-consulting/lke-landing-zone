package main

// Alert-rule SEMANTICS, as opposed to syntax.
//
// check-prom-rules proves the expressions parse. That is not the property that
// matters here: a rule can be live, loaded, syntactically perfect and still not
// fire when the thing it is named for happens. The reconciler's registry never
// expires a gauge, so a lane that dies keeps serving its last good sample —
// every `== 1` / `== 0` alert fed by it goes quiet exactly when its input
// breaks, which reads as "everything is fine".
//
// So the rules that exist to catch that get executed, against the CRD the
// cluster actually loads, via promtool's rule unit tests. See
// testdata/promrules/reconciler_alerts_test.yaml for the scenarios.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// reconcilerRuleCRD is the PrometheusRule under test, repo-relative.
const reconcilerRuleCRD = "../../../platform-apl/components/llzReconciler/llz-reconciler/prometheusrule.yaml"

func TestReconcilerAlertSemantics(t *testing.T) {
	promtool, err := exec.LookPath("promtool")
	if err != nil {
		// CI always has promtool — check-prom-rules is a hard gate and shells out
		// to it — so this skip only ever fires on a dev box without it.
		t.Skip("promtool not on PATH; the check-prom-rules gate covers CI")
	}

	crd, err := os.ReadFile(reconcilerRuleCRD)
	if err != nil {
		t.Fatalf("read PrometheusRule: %v", err)
	}
	// Run against the SHIPPED rules, extracted the same way the gate does — a
	// hand-copied duplicate would drift and prove nothing about production.
	bare, err := extractBareGroups(crd)
	if err != nil {
		t.Fatalf("extract spec.groups: %v", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rules.yml"), bare, 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := os.ReadFile("testdata/promrules/reconciler_alerts_test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	testFile := filepath.Join(dir, "alerts_test.yml")
	if err := os.WriteFile(testFile, cases, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(promtool, "test", "rules", testFile).CombinedOutput()
	if err != nil {
		t.Fatalf("promtool test rules failed: %v\n%s", err, out)
	}
	t.Logf("promtool:\n%s", out)
}

// Every credential alert must be NAMED so the job that reads credential alerts
// actually evaluates it.
//
// The daily credential-single-pane job runs
// `llz ci alert-eval --match '^LLZ(Token|Certificate|Credential)'`, so the alert
// name is not cosmetic — it is the filter. `LLZRootTokenParked` (the original
// spelling) is about the highest-privilege credential in the platform and
// matched NOTHING: the rule was live and would have fired through Alertmanager,
// but the job whose entire purpose is reading credential alerts skipped it.
//
// Asserted against the workflow's own regex, read from the file, so the two
// cannot drift apart — they are edited by different changes in different repos'
// worth of context.
func TestCredentialAlertsMatchTheSinglePaneFilter(t *testing.T) {
	wf, err := os.ReadFile("../../../instance-template/.github/workflows/llz-scheduled-checks.yml")
	if err != nil {
		t.Fatalf("read scheduled-checks workflow: %v", err)
	}
	m := regexp.MustCompile(`alert-eval --match '([^']+)'`).FindSubmatch(wf)
	if m == nil {
		t.Fatal("could not find the alert-eval --match filter in the workflow — this guard would pass vacuously")
	}
	filter := regexp.MustCompile(string(m[1]))

	crd, err := os.ReadFile(reconcilerRuleCRD)
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`(?m)^\s*-\s*alert:\s*(LLZ\S+)`).FindAllStringSubmatch(string(crd), -1)
	if len(names) == 0 {
		t.Fatal("found no alert names in the PrometheusRule")
	}
	checked := 0
	for _, n := range names {
		name := n[1]
		// Only the credential family is in this job's remit; the reconciler's own
		// health alerts are read by a different check.
		if !strings.Contains(name, "Credential") && !strings.Contains(name, "Token") {
			continue
		}
		checked++
		if !filter.MatchString(name) {
			t.Errorf("alert %q is a credential alert but does not match the single-pane filter %q — "+
				"the daily job will never evaluate it. Name it LLZCredential…", name, m[1])
		}
	}
	if checked == 0 {
		t.Fatal("matched no credential alerts to check — the name heuristic has drifted")
	}
}
