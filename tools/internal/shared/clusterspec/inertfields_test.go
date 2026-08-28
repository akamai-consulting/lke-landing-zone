package clusterspec

// The RATCHET on this list — "does the field still render nowhere?" — lives in
// the render package, because renderTargets is what decides that answer and
// asking clusterspec to predict its own renderer is the second-copy problem this
// area keeps producing.
//
// What lives HERE is the half that is clusterspec's own: each row's Probe (does
// this instance set the field?) and Marker (write a value the ratchet can hunt
// for) agreeing with the spec types they read. Those two are what decide whether
// an operator who configured Slack is TOLD it reaches nothing, and a Probe that
// silently stopped matching its own struct field would restore exactly the
// silence the list exists to break.

import (
	"strings"
	"testing"
)

// inertSpec is a minimal two-env instance — enough for the component-scoped rows
// to have somewhere to write.
func inertSpec(t *testing.T) *LandingZone {
	t.Helper()
	return &LandingZone{Spec: Spec{
		Environments: map[string]Environment{
			"prod":    {Components: map[string]ComponentToggle{}},
			"staging": {Components: map[string]ComponentToggle{}},
		},
	}}
}

// EVERY ROW'S PROBE MUST RECOGNISE ITS OWN MARKER. Derived from the list rather
// than written per row, so a sixth entry is covered the day it lands — and so a
// Probe that stops matching the struct field its Marker writes (a rename, a type
// change) fails here instead of going quiet.
func TestEveryProbeRecognisesItsOwnMarker(t *testing.T) {
	fields := InertSpecFields()
	if len(fields) == 0 {
		t.Skip("no inert fields declared — the goal state, and nothing to check")
	}
	for _, f := range fields {
		t.Run(f.Path, func(t *testing.T) {
			lz := inertSpec(t)
			if f.Probe(lz) {
				t.Fatal("the probe fires on a spec that sets nothing — it would report every " +
					"instance, and a finding printed unconditionally is one nobody reads")
			}
			f.Marker(lz)
			if !f.Probe(lz) {
				t.Error("the probe does not recognise what its own Marker writes — an operator " +
					"who sets this field is told nothing, which is the silence this list exists to break")
			}
		})
	}
}

// A CLEAN SPEC PRODUCES NO FINDINGS. The common case, and the one that decides
// whether the signal survives: doctor prints this section only when it applies.
func TestInertFindingsAreEmptyForACleanSpec(t *testing.T) {
	if got := InertFindings(inertSpec(t)); len(got) != 0 {
		t.Errorf("a spec that sets no inert field produced %d finding(s):\n%s",
			len(got), strings.Join(got, "\n"))
	}
}

// …and a finding names the field AND why it is inert. The "why" is the half an
// operator needs to decide whether to keep the setting; without it the finding is
// an accusation with no next step.
func TestAFindingNamesThePathAndTheReason(t *testing.T) {
	for _, f := range InertSpecFields() {
		t.Run(f.Path, func(t *testing.T) {
			lz := inertSpec(t)
			f.Marker(lz)
			var line string
			for _, l := range InertFindings(lz) {
				if strings.Contains(l, f.Path) {
					line = l
				}
			}
			if line == "" {
				t.Fatalf("no finding names %q", f.Path)
			}
			if !strings.Contains(line, "reaches NO cluster") {
				t.Errorf("the finding does not say the setting reaches nothing:\n%s", line)
			}
			if !strings.Contains(line, f.Why) {
				t.Errorf("the finding drops the reason, leaving an accusation with no next step:\n%s", line)
			}
			if !strings.Contains(line, "upstream-asks.md") {
				t.Errorf("the finding does not point at where the gap is tracked:\n%s", line)
			}
		})
	}
}

// `receivers: [none]` is the DEFAULT, and must not be read as "the operator
// configured alerting". Reporting it would put a finding on every stock instance.
func TestTheDefaultReceiverSetIsNotAFinding(t *testing.T) {
	lz := inertSpec(t)
	lz.Spec.Alerting.Receivers = []string{"none"}
	if got := InertFindings(lz); len(got) != 0 {
		t.Errorf("the default [none] receiver set produced a finding:\n%s", strings.Join(got, "\n"))
	}
}

// Each sizing knob independently triggers its row. Without this, a Probe that
// checked only `retention` would satisfy the marker test above — the Marker sets
// several at once — while missing an instance that set only `registryStorage`.
func TestEachSizingKnobIsSeenOnItsOwn(t *testing.T) {
	replicas := 3
	for name, set := range map[string]func(*LandingZone){
		"observability.retention": func(lz *LandingZone) {
			c := lz.Spec.Environments["prod"].Components
			c["observability"] = ComponentToggle{Retention: "30d"}
		},
		"observability.storage": func(lz *LandingZone) {
			c := lz.Spec.Environments["prod"].Components
			c["observability"] = ComponentToggle{Storage: "50Gi"}
		},
		"observability.replicas": func(lz *LandingZone) {
			c := lz.Spec.Environments["prod"].Components
			c["observability"] = ComponentToggle{Replicas: &replicas}
		},
		"harbor.registryStorage": func(lz *LandingZone) {
			c := lz.Spec.Environments["prod"].Components
			c["harbor"] = ComponentToggle{RegistryStorage: "100Gi"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			lz := inertSpec(t)
			set(lz)
			if len(InertFindings(lz)) == 0 {
				t.Errorf("setting %s alone produced no finding — an instance that sets only this "+
					"knob is told it works", name)
			}
		})
	}
}

// A Slack CHANNEL with no receiver still counts: it is a configured intention,
// and the operator deserves to know it is going nowhere.
func TestASlackChannelAloneIsAFinding(t *testing.T) {
	lz := inertSpec(t)
	lz.Spec.Alerting.Slack.Channel = "platform-alerts"
	if len(InertFindings(lz)) == 0 {
		t.Error("a configured Slack channel with no receiver produced no finding")
	}
}

// A component this repo has no inert knobs for must not trigger the row. Without
// this, a Probe that walked every component and checked "any field set" would
// pass every other test here while reporting unrelated instances.
func TestAnUnrelatedComponentDoesNotTriggerTheRow(t *testing.T) {
	lz := inertSpec(t)
	lz.Spec.Environments["prod"].Components["openbao"] = ComponentToggle{Retention: "30d"}
	if got := InertFindings(lz); len(got) != 0 {
		t.Errorf("a knob on a component with no inert row produced a finding:\n%s",
			strings.Join(got, "\n"))
	}
}

// Every row is well-formed. A row with no Probe or no Marker would panic the
// ratchet in the render package rather than fail it readably.
func TestEveryRowIsWellFormed(t *testing.T) {
	for _, f := range InertSpecFields() {
		if strings.TrimSpace(f.Path) == "" {
			t.Error("a row has no Path — its finding would name nothing")
		}
		if strings.TrimSpace(f.Why) == "" {
			t.Errorf("%q has no reason", f.Path)
		}
		if f.Probe == nil {
			t.Errorf("%q has no Probe — doctor could never report it", f.Path)
		}
		if f.Marker == nil {
			t.Errorf("%q has no Marker — the render-side ratchet cannot check it", f.Path)
		}
	}
}
