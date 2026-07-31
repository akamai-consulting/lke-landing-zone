package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExpectedRotationCredsComesFromCredPaths(t *testing.T) {
	got := expectedRotationCreds()
	if len(got) != len(credPaths) {
		t.Fatalf("expected one entry per credPaths row, got %d vs %d", len(got), len(credPaths))
	}
	// Derived from the DECLARATION. If this ever started from the metrics, the
	// gate would be asking Prometheus which credentials exist and then checking
	// those exist — a tautology that passes on the missing-series bug.
	byCred := map[string]string{}
	for _, e := range got {
		byCred[e.Cred] = e.Class
	}
	for _, cp := range credPaths {
		if byCred[cp.cred] != cp.class {
			t.Errorf("credPaths declares %s as %q; expected set has %q", cp.cred, cp.class, byCred[cp.cred])
		}
	}
}

func TestSlaForClass(t *testing.T) {
	if slaForClass(credClassAutomated) != rotationSLAAlertableDays {
		t.Error("automated must carry the 90d SLA")
	}
	if slaForClass(credClassOnDemand) != rotationSLAAlertableDays {
		t.Error("on-demand shares the 90d SLA — the age is actionable, someone can dispatch the workflow")
	}
	for _, c := range []string{credClassGenerateOnce, credClassTracksSource, credClassStatic} {
		if slaForClass(c) != rotationSLAInfoDays {
			t.Errorf("%s must carry the yearly info threshold, not the 90d SLA", c)
		}
	}
}

func TestEvalRotationHealth(t *testing.T) {
	expected := []expectedCred{
		{"linode-incluster-pat", credClassAutomated, false},
		{"db-admin", credClassOnDemand, false},
		{"grafana-admin", credClassGenerateOnce, false},
		{"missing-automated", credClassAutomated, false},
		{"unseeded-static", credClassStatic, false},
	}
	ages := map[string]float64{
		"linode-incluster-pat": 30,
		"db-admin":             120, // past the 90d SLA
		"grafana-admin":        400, // past 365 but NOT gated by default
	}
	vs := evalRotationHealth(expected, ages, false)
	by := map[string]credVerdict{}
	for _, v := range vs {
		by[v.Cred] = v
	}

	if by["linode-incluster-pat"].FailWhy != "" {
		t.Errorf("a fresh automated credential must pass: %s", by["linode-incluster-pat"].FailWhy)
	}
	// An on-demand breach means nobody TRIGGERED it — a different remedy from a
	// broken rotator, and the message must say so.
	if f := by["db-admin"].FailWhy; f == "" || !strings.Contains(f, "TRIGGERED") {
		t.Errorf("an overdue on-demand credential must fail naming the remedy, got %q", f)
	}
	// Non-alertable classes are reported, never gated: nothing lowers their age,
	// so gating would be a permanent red that trains people to ignore the gate.
	if by["grafana-admin"].FailWhy != "" {
		t.Errorf("a generate-once credential past 365d must NOT gate by default: %s", by["grafana-admin"].FailWhy)
	}
	// THE case the gate exists for: declared, alertable, publishing nothing.
	f := by["missing-automated"].FailWhy
	if f == "" {
		t.Fatal("a declared alertable credential with NO series must fail — no alert can ever fire for it")
	}
	if !strings.Contains(f, "never evaluates") {
		t.Errorf("the failure must explain that an absent series makes the alert unfireable, got %q", f)
	}
	// An unseeded optional path legitimately 404s at sample time and must not fail.
	if by["unseeded-static"].FailWhy != "" {
		t.Errorf("an unseeded non-alertable path must be skipped, not failed: %s", by["unseeded-static"].FailWhy)
	}
}

// An OPT-IN path is alertable — it carries a real 90d SLA — but most deployments
// never seed it, so its ABSENCE is the normal state and cannot be a finding. The
// first release-e2e to reach this gate failed on exactly this: every seeded
// credential reported OK and linode-cloud-firewall, which the e2e cluster
// correctly does not have, reded the lane.
//
// Both halves are asserted here, because fixing this by demoting the class would
// have passed the first half and silently dropped the SLA.
func TestEvalRotationHealthOptionalPathAbsentIsNotAFinding(t *testing.T) {
	expected := []expectedCred{
		{"linode-cloud-firewall", credClassOnDemand, true},
		{"required-on-demand", credClassOnDemand, false},
	}
	vs := evalRotationHealth(expected, map[string]float64{}, false)
	by := map[string]credVerdict{}
	for _, v := range vs {
		by[v.Cred] = v
	}
	if f := by["linode-cloud-firewall"].FailWhy; f != "" {
		t.Errorf("an unseeded OPT-IN path must be skipped, not failed: %s", f)
	}
	if by["required-on-demand"].FailWhy == "" {
		t.Error("a REQUIRED on-demand credential with no series must still fail — that is what this gate is for")
	}
	// The SLA must survive the exemption: once seeded, an overdue opt-in path is
	// still a finding.
	overdue := evalRotationHealth(expected, map[string]float64{"linode-cloud-firewall": 200}, false)[0]
	if overdue.FailWhy == "" {
		t.Error("an opt-in path that IS seeded and is past its SLA must fail — the exemption is about presence, not age")
	}
}

// The opt-in exemption must be spelled in credPaths, not hardcoded in the gate:
// the sampler and the gate read the same table, and a second list would be the
// split contract docs/e2e-gates.md is about.
func TestCredPathsMarksTheOptInFirewallTokenOptional(t *testing.T) {
	var seen bool
	for _, cp := range credPaths {
		if cp.cred != "linode-cloud-firewall" {
			if cp.optional {
				t.Errorf("%s is marked optional; every other declared path is seeded on every deployment", cp.cred)
			}
			continue
		}
		seen = true
		if !cp.optional {
			t.Error("linode-cloud-firewall is the documented OPT-IN least-privilege token — it must be optional")
		}
		if cp.class != credClassOnDemand {
			t.Errorf("linode-cloud-firewall class = %s, want on-demand — demoting it drops the 90d SLA the docs promise", cp.class)
		}
	}
	if !seen {
		t.Fatal("linode-cloud-firewall is no longer in credPaths")
	}
}

func TestEvalRotationHealthStrictGatesInfoClasses(t *testing.T) {
	expected := []expectedCred{{"grafana-admin", credClassGenerateOnce, false}}
	ages := map[string]float64{"grafana-admin": 400}
	if v := evalRotationHealth(expected, ages, true)[0]; v.FailWhy == "" {
		t.Error("--strict must gate the 365d info threshold too")
	}
	if v := evalRotationHealth(expected, map[string]float64{"grafana-admin": 10}, true)[0]; v.FailWhy != "" {
		t.Errorf("a fresh credential must pass even under --strict: %s", v.FailWhy)
	}
}

func TestRunAssertRotationHealthFailsOnMissingSeries(t *testing.T) {
	orig := withPrometheus
	t.Cleanup(func() { withPrometheus = orig })
	// Prometheus answers with an EMPTY vector: every declared credential is
	// unobserved. The alertable ones must fail.
	withPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(string) ([]byte, error) {
			return []byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`), nil
		})
	}
	err := runCIAssertRotationHealth("ns/prom:9090", "llz-reconciler", false, 0, time.Millisecond)
	if err == nil {
		t.Fatal("no credential-age series at all must fail the gate")
	}
}

// A Prometheus failure must not be reported as "the credentials are unobserved" —
// the same could-not-ask-is-not-an-answer split the other gauge gates make.
func TestRunAssertRotationHealthFailsOnUnreachablePrometheus(t *testing.T) {
	orig := withPrometheus
	t.Cleanup(func() { withPrometheus = orig })
	withPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(string) ([]byte, error) {
			return []byte(`{"status":"error","error":"query timed out"}`), nil
		})
	}
	err := runCIAssertRotationHealth("ns/prom:9090", "llz-reconciler", false, 0, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "could not reach Prometheus") {
		t.Errorf("a query failure must be reported as a query failure, got %v", err)
	}
}

// TestRotationSLAsMatchThePrometheusRules pins this gate's thresholds against the
// alert rules. A gate that disagreed with the alert would fail on credentials
// nobody is paged about, or pass on ones they are — and the two are edited in
// different files by different changes.
func TestRotationSLAsMatchThePrometheusRules(t *testing.T) {
	path := filepath.Join("..", "..", "..", "platform-apl", "components",
		"llzReconciler", "llz-reconciler", "prometheusrule.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("prometheusrule not reachable from the test cwd: %v", err)
	}
	body := string(raw)

	// Pull each llz_credential_age_days threshold together with the class matcher
	// it applies to, so the two SLAs are checked against the right rule.
	re := regexp.MustCompile(`class=~"([a-z|-]+)"[^\n]*?>\s*(\d+)`)
	matches := re.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatal("found no credential-age thresholds in the rules — this guard would pass vacuously")
	}

	seen := map[float64]bool{}
	for _, m := range matches {
		classes, thresholdStr := m[1], m[2]
		threshold, err := strconv.ParseFloat(thresholdStr, 64)
		if err != nil {
			t.Fatalf("unparseable threshold %q: %v", thresholdStr, err)
		}
		seen[threshold] = true

		// Every class named in the rule must exist here, and must map to this
		// rule's threshold.
		for _, c := range strings.Split(classes, "|") {
			if got := slaForClass(c); got != threshold {
				t.Errorf("rule alerts on class %q at >%v days, but slaForClass(%q) is %v — "+
					"the gate and the alert disagree about when this credential is overdue",
					c, threshold, c, got)
			}
			if alertableCredClasses[c] != (threshold == rotationSLAAlertableDays) {
				t.Errorf("class %q: alertable=%v here but the rule puts it at the %v-day threshold",
					c, alertableCredClasses[c], threshold)
			}
		}
	}
	for _, want := range []float64{rotationSLAAlertableDays, rotationSLAInfoDays} {
		if !seen[want] {
			t.Errorf("this gate uses a %v-day SLA that no alert rule mentions", want)
		}
	}
}

// Every class credPaths uses must be one this gate knows how to judge; an
// unknown class would silently fall through to the info threshold.
func TestEveryCredPathClassIsKnown(t *testing.T) {
	known := map[string]bool{
		credClassAutomated: true, credClassOnDemand: true,
		credClassGenerateOnce: true, credClassTracksSource: true, credClassStatic: true,
	}
	for _, cp := range credPaths {
		if !known[cp.class] {
			t.Errorf("credPaths entry %s carries class %q, which this gate does not know — "+
				"it would silently be judged against the yearly threshold", cp.cred, cp.class)
		}
	}
}
