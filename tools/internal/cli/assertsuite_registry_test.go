package cli

// assertsuite_registry_test.go — the assert battery and the registry must agree.
//
// ────────────────────────────────────────────────────────────────────────────
// THE BATTERY WAS A SECOND REGISTRY THAT NEVER CONSULTED THE FIRST.
//
// assert-suite has a lane table, parallel execution, gating vs report-only and a
// skip state — most of a driver. What it lacked was any relationship to the
// extension registry: seventeen lanes hand-typed against the declarations, with
// nothing comparing them. Two lists, one edited.
//
// The immediate cost was worse than drift. `Lane.Extension` existed, was
// documented as the field that stops a disabled lane looking like a passing one,
// and was NEVER SET on any lane — so runLane's `l.Extension != ""` guard was false
// every time and the entire enablement path was unreachable code behind a
// condition that could not fire.
//
// THESE TESTS LIVE IN internal/cli for the reason command_wiring_test.go does: the
// registry IMPORTS assertsuite in order to declare it, so a call the other way is
// a cycle. This package holds both. Before the tree left package main they could
// not have been written at all.
// ────────────────────────────────────────────────────────────────────────────

import (
	"sort"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertsuite"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension/registry"
)

// EVERY STEP MUST NAME A REGISTERED EXTENSION.
//
// An unnamed step is one enablement silently skips: the guard is
// `step.Extension != ""`, so the empty default means "always run" and a forgotten
// name is invisible — which is exactly how this path came to be dead. A misspelled
// name is worse than forgotten: EnabledFor cannot resolve it, so the step runs and
// the instance looks like it still has a capability it dropped.
func TestEveryBatteryStepNamesARegisteredExtension(t *testing.T) {
	known := map[string]bool{}
	for _, e := range registry.All() {
		known[e.Name] = true
	}

	var unnamed, unknown []string
	var steps int
	for _, l := range assertsuite.Lanes("us-ord") {
		for _, s := range l.Steps {
			steps++
			switch {
			case s.Extension == "":
				unnamed = append(unnamed, l.Name+"/"+strings.Join(s.Argv, " "))
			case !known[s.Extension]:
				unknown = append(unknown, l.Name+"/"+s.Extension)
			}
		}
	}
	// A census that found nothing agrees with any battery.
	if steps == 0 {
		t.Fatal("the battery reported no steps — this test would pass over an empty set")
	}
	sort.Strings(unnamed)
	sort.Strings(unknown)

	if len(unnamed) > 0 {
		t.Errorf("%d battery step(s) name no extension:\n\t%s\n"+
			"\tEnablement guards on a non-empty name, so an unnamed step ALWAYS runs and no "+
			"instance can turn it off. That default is what made this path dead code.",
			len(unnamed), strings.Join(unnamed, "\n\t"))
	}
	if len(unknown) > 0 {
		t.Errorf("%d battery step(s) name an extension the registry does not have:\n\t%s\n"+
			"\tEnabledFor cannot resolve it, so the step runs regardless — a typo reads "+
			"exactly like a capability the instance still has.",
			len(unknown), strings.Join(unknown, "\n\t"))
	}
	t.Logf("battery: %d step(s), all owned", steps)
}

// batteryExempt names extensions that declare `assertion:verified` and are
// deliberately NOT lanes, with the argument. A bool would not do: an exemption
// without a reason is indistinguishable from a lane nobody wrote.
var batteryExempt = map[string]string{
	"assert-suite": "it IS the battery; a lane running the battery would recurse",

	// Its verified evidence arrives through another lane rather than being absent.
	// assert-storage's checks are reconciler lanes that run continuously, and
	// `scrape-reconciler` runs assert-reconciler-effects, which is defined as "each
	// enabled reconciler lane's cluster invariant actually holds". A second lane
	// would re-assert the same invariant from the same gauges.
	"assert-storage": "covered through assert-reconciler-effects in the scrape-reconciler lane",

	// Documented at length in the `credentials` lane's own Why, and the reason is
	// sequencing rather than coverage: the db-admin credential lives only in
	// OpenBao (no ExternalSecret materialises it) and the bootstrap job REVOKES the
	// root token before this suite runs — so a lane here would fail for want of a
	// token rather than for anything about the database. It runs as its own step
	// earlier in the job, while the token still exists.
	"database-provisioner": "the root token is revoked before this suite runs; it gates earlier in the job",
}

// EVERY `assertion:verified` EXTENSION IS A LANE, OR IS EXEMPT WITH A REASON.
//
// THE RULE COMES FROM THE MODEL RATHER THAN FROM JUDGEMENT, which is what makes it
// checkable. The battery's whole job is to decide whether `verified` holds, and
// `verified` is defined as the state whose assertions hold. So an extension
// declaring assertion:verified and contributing nothing to the battery is either
// a lane nobody wrote, or an exemption nobody argued.
//
// It deliberately does NOT require the converse. Two lanes run extensions with no
// assertion:verified binding, both for recorded reasons: `assert-reconciler`
// declares assertion:operating (the reconciler is a steady-state thing that the
// battery samples), and `assert-objstore` declares transition:converged alone
// because an assertion may hold read grants only and Transition cannot reach
// `verified` — its own extension.go argues that forced move at length and files it
// under the unresolved fifth-kind question. Requiring the converse would demand
// those declarations change, which is a design decision this test has no business
// making as a side effect.
func TestEveryVerifiedAssertionIsInTheBattery(t *testing.T) {
	inBattery := map[string]bool{}
	for _, l := range assertsuite.Lanes("us-ord") {
		for _, s := range l.Steps {
			inBattery[s.Extension] = true
		}
	}

	var missing []string
	declares := map[string]bool{}
	for _, e := range registry.All() {
		for _, b := range e.Bindings {
			if b.Kind != extension.Assertion || b.State != extension.Verified {
				continue
			}
			declares[e.Name] = true
			if inBattery[e.Name] {
				continue
			}
			if _, ok := batteryExempt[e.Name]; ok {
				continue
			}
			missing = append(missing, e.Name)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("%d extension(s) declare assertion:verified but contribute nothing to the "+
			"battery:\n\t%s\n"+
			"\t`verified` is the state whose required assertions hold, so an assertion that "+
			"never runs is evidence the battery claims and does not have. Add a lane, or add "+
			"it to batteryExempt with the argument.",
			len(missing), strings.Join(missing, "\n\t"))
	}

	// The ratchet half. An exemption for an extension that is now a lane — or that
	// stopped declaring assertion:verified — is a stale allowance, and a stale
	// allowance is what the next lane gets written to match.
	var stale []string
	for name, why := range batteryExempt {
		if inBattery[name] || !declares[name] {
			stale = append(stale, name)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("batteryExempt[%q] has no reason", name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%d exemption(s) no longer apply — DELETE them from batteryExempt:\n\t%s",
			len(stale), strings.Join(stale, "\n\t"))
	}

	t.Logf("assertion:verified extensions: %d, in battery: %d, exempt: %d",
		len(declares), len(inBattery), len(batteryExempt))
}
