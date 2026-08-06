package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/promwire"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/reconcilelanes"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tokeninv"
)

func TestExpectedRotationCredsComesFromCredPaths(t *testing.T) {
	got := expectedRotationCreds()
	if len(got) != len(reconcilelanes.CredPaths) {
		t.Fatalf("expected one entry per reconcilelanes.CredPaths row, got %d vs %d", len(got), len(reconcilelanes.CredPaths))
	}
	// Derived from the DECLARATION. If this ever started from the metrics, the
	// gate would be asking Prometheus which credentials exist and then checking
	// those exist — a tautology that passes on the missing-series bug.
	byCred := map[string]string{}
	for _, e := range got {
		byCred[e.Cred] = e.Class
	}
	for _, cp := range reconcilelanes.CredPaths {
		if byCred[cp.Cred] != cp.Class {
			t.Errorf("reconcilelanes.CredPaths declares %s as %q; expected set has %q", cp.Cred, cp.Class, byCred[cp.Cred])
		}
	}
}

func TestSlaForClass(t *testing.T) {
	if slaForClass(reconcilelanes.CredClassAutomated) != rotationSLAAlertableDays {
		t.Error("automated must carry the 90d SLA")
	}
	if slaForClass(reconcilelanes.CredClassOnDemand) != rotationSLAAlertableDays {
		t.Error("on-demand shares the 90d SLA — the age is actionable, someone can dispatch the workflow")
	}
	for _, c := range []string{reconcilelanes.CredClassGenerateOnce, reconcilelanes.CredClassTracksSource, reconcilelanes.CredClassStatic} {
		if slaForClass(c) != rotationSLAInfoDays {
			t.Errorf("%s must carry the yearly info threshold, not the 90d SLA", c)
		}
	}
}

func TestEvalRotationHealth(t *testing.T) {
	expected := []expectedCred{
		{"linode-incluster-pat", reconcilelanes.CredClassAutomated, false},
		{"db-admin", reconcilelanes.CredClassOnDemand, false},
		{"grafana-admin", reconcilelanes.CredClassGenerateOnce, false},
		{"missing-automated", reconcilelanes.CredClassAutomated, false},
		{"unseeded-static", reconcilelanes.CredClassStatic, false},
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
	// so gating would be a permanent color.Red that trains people to ignore the gate.
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
		{"linode-cloud-firewall", reconcilelanes.CredClassOnDemand, true},
		{"required-on-demand", reconcilelanes.CredClassOnDemand, false},
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

// The opt-in exemption must be spelled in reconcilelanes.CredPaths, not hardcoded in the gate:
// the sampler and the gate read the same table, and a second list would be the
// split contract docs/e2e-gates.md is about.
func TestCredPathsMarksTheOptInFirewallTokenOptional(t *testing.T) {
	var seen bool
	for _, cp := range reconcilelanes.CredPaths {
		if cp.Cred != "linode-cloud-firewall" {
			if cp.Optional {
				t.Errorf("%s is marked optional; every other declared path is seeded on every deployment", cp.Cred)
			}
			continue
		}
		seen = true
		if !cp.Optional {
			t.Error("linode-cloud-firewall is the documented OPT-IN least-privilege token — it must be optional")
		}
		if cp.Class != reconcilelanes.CredClassOnDemand {
			t.Errorf("linode-cloud-firewall class = %s, want on-demand — demoting it drops the 90d SLA the docs promise", cp.Class)
		}
	}
	if !seen {
		t.Fatal("linode-cloud-firewall is no longer in reconcilelanes.CredPaths")
	}
}

func TestEvalRotationHealthStrictGatesInfoClasses(t *testing.T) {
	expected := []expectedCred{{"grafana-admin", reconcilelanes.CredClassGenerateOnce, false}}
	ages := map[string]float64{"grafana-admin": 400}
	if v := evalRotationHealth(expected, ages, true)[0]; v.FailWhy == "" {
		t.Error("--strict must gate the 365d info threshold too")
	}
	if v := evalRotationHealth(expected, map[string]float64{"grafana-admin": 10}, true)[0]; v.FailWhy != "" {
		t.Errorf("a fresh credential must pass even under --strict: %s", v.FailWhy)
	}
}

func TestRunAssertRotationHealthFailsOnMissingSeries(t *testing.T) {
	orig := assertobs.WithPrometheus
	t.Cleanup(func() { assertobs.WithPrometheus = orig })
	// Prometheus answers with an EMPTY vector: every declared credential is
	// unobserved. The alertable ones must fail.
	assertobs.WithPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(string) ([]byte, error) {
			return []byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`), nil
		})
	}
	err := runCIAssertRotationHealth("ns/prom:9090", "llz-reconciler", false, false, 0, time.Millisecond)
	if err == nil {
		t.Fatal("no credential-age series at all must fail the gate")
	}
}

// A Prometheus failure must not be reported as "the credentials are unobserved" —
// the same could-not-ask-is-not-an-answer split the other gauge gates make.
func TestRunAssertRotationHealthFailsOnUnreachablePrometheus(t *testing.T) {
	orig := assertobs.WithPrometheus
	t.Cleanup(func() { assertobs.WithPrometheus = orig })
	assertobs.WithPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(string) ([]byte, error) {
			return []byte(`{"status":"error","error":"query timed out"}`), nil
		})
	}
	err := runCIAssertRotationHealth("ns/prom:9090", "llz-reconciler", false, false, 0, time.Millisecond)
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

// Every class reconcilelanes.CredPaths uses must be one this gate knows how to judge; an
// unknown class would silently fall through to the info threshold.
func TestEveryCredPathClassIsKnown(t *testing.T) {
	known := map[string]bool{
		reconcilelanes.CredClassAutomated: true, reconcilelanes.CredClassOnDemand: true,
		reconcilelanes.CredClassGenerateOnce: true, reconcilelanes.CredClassTracksSource: true, reconcilelanes.CredClassStatic: true,
	}
	for _, cp := range reconcilelanes.CredPaths {
		if !known[cp.Class] {
			t.Errorf("reconcilelanes.CredPaths entry %s carries class %q, which this gate does not know — "+
				"it would silently be judged against the yearly threshold", cp.Cred, cp.Class)
		}
	}
}

// ── the presence lane ────────────────────────────────────────────────────────

// The steady state: probe authenticated, everything expected present is present,
// and the one credential expected ABSENT is absent.
func presenceSteadyState() map[string]float64 {
	m := map[string]float64{}
	for _, t := range tokeninv.GHSecretTargets {
		v := 1.0
		if t.Expect == tokeninv.CredExpectAbsent {
			v = 0
		}
		m[credLabelForSecret(t.Name)] = v
	}
	return m
}

func TestPresenceHealthPassesInSteadyState(t *testing.T) {
	for _, v := range evalPresenceHealth(presenceSteadyState(), 1, true, false) {
		if v.FailWhy != "" {
			t.Errorf("%s should pass: %s", v.Cred, v.FailWhy)
		}
	}
}

// The failure the age lane structurally cannot see. A credential that was never
// configured has no age, so llz_credential_age_days has nothing for it and every
// age check — gate and alert alike — silently skips it.
func TestPresenceHealthFailsOnAnUnconfiguredCredential(t *testing.T) {
	m := presenceSteadyState()
	m["openbao-recovery-key-2"] = 0
	var got string
	for _, v := range evalPresenceHealth(m, 1, true, false) {
		if v.Cred == "openbao-recovery-key-2" {
			got = v.FailWhy
		}
	}
	if got == "" {
		t.Fatal("an unconfigured credential must fail the presence lane")
	}
	if !strings.Contains(got, "no age because it has no value") {
		t.Errorf("the message must explain why the age lane cannot catch it: %q", got)
	}
}

// The same series read the other way. A root token is ephemeral by design, so a
// SET one is the finding — a live full-admin credential left behind by a
// break-glass whose revoke never ran.
func TestPresenceHealthFailsOnAParkedRootToken(t *testing.T) {
	m := presenceSteadyState()
	m["openbao-root-token"] = 1
	var got string
	for _, v := range evalPresenceHealth(m, 1, true, false) {
		if v.Cred == "openbao-root-token" {
			got = v.FailWhy
		}
	}
	if got == "" {
		t.Fatal("a parked root token must fail the presence lane")
	}
	if !strings.Contains(got, "action=revoke") {
		t.Errorf("the message must name the remedy: %q", got)
	}
}

// The probe reporting failure is a lane-wide break, not a per-credential one.
func TestPresenceHealthFailsWhenTheProbeCouldNotAuthenticate(t *testing.T) {
	var funnel credVerdict
	for _, v := range evalPresenceHealth(presenceSteadyState(), 0, true, false) {
		if v.Cred == "token-inventory" {
			funnel = v
		}
	}
	if funnel.FailWhy == "" {
		t.Fatalf("probe_ok=0 must fail the funnel verdict, got %+v", funnel)
	}
	if !strings.Contains(funnel.FailWhy, "GH_REPO") {
		t.Errorf("the message must name the missing input: %q", funnel.FailWhy)
	}
}

// A freshly bootstrapped cluster has not run the scheduled writer, so gating
// there would fail the e2e suite on a job that legitimately has not run. The skip
// is reported rather than silent, and --require-inventory turns it into a failure
// for the caller that runs the writer moments earlier.
func TestPresenceHealthSkipsWhenTheWriterHasNeverRun(t *testing.T) {
	vs := evalPresenceHealth(nil, 0, false, false)
	if len(vs) != 1 || vs[0].FailWhy != "" || vs[0].Present {
		t.Fatalf("an unprimed cluster must skip, not fail: %+v", vs)
	}
	vs = evalPresenceHealth(nil, 0, false, true)
	if len(vs) != 1 || vs[0].FailWhy == "" {
		t.Fatalf("--require-inventory must turn the skip into a failure: %+v", vs)
	}
}

// "The probe said OK and yet this credential has no series" is a funnel defect,
// distinct from "the credential is absent" — different cause, different remedy,
// so the messages must not be interchangeable.
func TestPresenceHealthDistinguishesAMissingSeriesFromAnAbsentCredential(t *testing.T) {
	m := presenceSteadyState()
	delete(m, "openbao-seal-key")
	for _, v := range evalPresenceHealth(m, 1, true, false) {
		if v.Cred != "openbao-seal-key" {
			continue
		}
		if !strings.Contains(v.FailWhy, "NOT evidence the credential is missing") {
			t.Errorf("a missing series must not be diagnosed as an absent credential: %q", v.FailWhy)
		}
		if !strings.Contains(v.FailWhy, "403") {
			t.Errorf("the message must name the likely cause — a refused environment-scope read: %q", v.FailWhy)
		}
	}
}

// Presence verdicts carry no age, and must not be printed as though they did:
// "0 days old, SLA 90" on a credential whose problem is that it does not exist
// reads as a passing measurement.
func TestPresenceVerdictsAreMarkedAsSuch(t *testing.T) {
	for _, v := range evalPresenceHealth(presenceSteadyState(), 1, true, false) {
		if v.Lane != presenceLane {
			t.Errorf("%s: Lane = %q, want %q", v.Cred, v.Lane, presenceLane)
		}
		if v.Age != 0 {
			t.Errorf("%s: presence verdicts must carry no age, got %v", v.Cred, v.Age)
		}
	}
}

// The Harbor robot pair is legitimately absent on a standby peer until the ACTIVE
// peer's provisioner has published it, and on any deployment before Harbor first
// comes up. Gating on it — as the first draft did, classing both `present` —
// would fail the daily credential job on a healthy standby, which is a worse
// outcome than the gap it was meant to close.
func TestPresenceHealthDoesNotGateOptionalCredentials(t *testing.T) {
	m := presenceSteadyState()
	var optional []string
	for _, tgt := range tokeninv.GHSecretTargets {
		if tgt.Expect == tokeninv.CredExpectOptional {
			optional = append(optional, credLabelForSecret(tgt.Name))
			delete(m, credLabelForSecret(tgt.Name)) // absent AND publishing nothing
		}
	}
	if len(optional) == 0 {
		t.Skip("no optional targets declared")
	}
	for _, v := range evalPresenceHealth(m, 1, true, false) {
		for _, o := range optional {
			if v.Cred == o && (v.FailWhy != "" || v.Gated) {
				t.Errorf("%s is optional and must not gate: FailWhy=%q Gated=%v", v.Cred, v.FailWhy, v.Gated)
			}
		}
	}
}

// The seam the earlier presence tests never crossed. They exercised
// evalPresenceHealth — the pure half — and proved nothing about whether the
// probe gauge could be READ, which is where the lane was actually broken:
// llz_credential_secret_probe_ok has no `cred` label, and promVectorByLabel drops
// every sample whose label value is empty. probeSeen was permanently false, so
// the lane silently checked nothing and --require-inventory would have failed
// every run on every cluster.
func TestPromFirstSampleReadsALabellessGauge(t *testing.T) {
	raw := []byte(`{"status":"success","data":{"resultType":"vector","result":[
	  {"metric":{"__name__":"llz_credential_secret_probe_ok","namespace":"llz-reconciler"},"value":[1,"0"]}
	]}}`)
	// The helper the rest of this file uses cannot see it — pinned so the reason
	// promFirstSample exists is visible rather than folklore.
	if m, err := promwire.VectorByLabel(raw, "cred"); err != nil || len(m) != 0 {
		t.Fatalf("promVectorByLabel is expected to drop a label-less sample, got %v (%v)", m, err)
	}
	v, ok, err := promFirstSample(raw)
	if err != nil || !ok || v != 0 {
		t.Fatalf("promFirstSample = (%v, %v, %v), want (0, true, nil)", v, ok, err)
	}
}

func TestPromFirstSampleDistinguishesEmptyFromError(t *testing.T) {
	empty := []byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`)
	if v, ok, err := promFirstSample(empty); err != nil || ok || v != 0 {
		t.Errorf("an empty result is 'no series yet', got (%v, %v, %v)", v, ok, err)
	}
	bad := []byte(`{"status":"error","error":"query timed out"}`)
	if _, ok, err := promFirstSample(bad); err == nil || ok {
		t.Errorf("a query error must be an error, not an absence: ok=%v err=%v", ok, err)
	}
	if _, _, err := promFirstSample([]byte("not json")); err == nil {
		t.Error("unparseable input must error")
	}
}

// End to end through the parsing seam: a healthy funnel must be SEEN, so the
// lane actually evaluates. The previous implementation passed every pure test
// while failing this one.
func TestProbePresenceHealthSeesAHealthyFunnel(t *testing.T) {
	orig := assertobs.WithPrometheus
	t.Cleanup(func() { assertobs.WithPrometheus = orig })
	assertobs.WithPrometheus = func(_ string, fn func(func(string) ([]byte, error)) error) error {
		return fn(func(q string) ([]byte, error) {
			if strings.Contains(q, "secret_probe_ok") {
				return []byte(`{"status":"success","data":{"resultType":"vector","result":[
				  {"metric":{"namespace":"llz-reconciler"},"value":[1,"1"]}]}}`), nil
			}
			var rows []string
			for _, tgt := range tokeninv.GHSecretTargets {
				v := "1"
				if tgt.Expect == tokeninv.CredExpectAbsent {
					v = "0"
				}
				rows = append(rows, `{"metric":{"cred":"`+credLabelForSecret(tgt.Name)+`"},"value":[1,"`+v+`"]}`)
			}
			return []byte(`{"status":"success","data":{"resultType":"vector","result":[` +
				strings.Join(rows, ",") + `]}}`), nil
		})
	}
	vs, err := probePresenceHealth("ns/prom:9090", "llz-reconciler", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) == 1 && vs[0].Cred == "token-inventory" && !vs[0].Present {
		t.Fatal("a healthy funnel was read as 'the writer has never run' — the probe gauge is not being parsed")
	}
	for _, v := range vs {
		if v.FailWhy != "" {
			t.Errorf("%s must pass on a healthy funnel: %s", v.Cred, v.FailWhy)
		}
	}
}
