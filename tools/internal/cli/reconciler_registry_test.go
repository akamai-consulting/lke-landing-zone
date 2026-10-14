package cli

// reconciler_registry_test.go — the reconcile daemon's lanes and the registry's
// invariant bindings must be the same set.
//
// ────────────────────────────────────────────────────────────────────────────
// THE LAST OF THE FOUR BINDING KINDS TO GET A COUPLING, AND IT HAD ALREADY
// DRIFTED WHEN IT ARRIVED.
//
// Gates dispatch from the registry. Assertions are checked against it
// (assertsuite_registry_test.go). Invariants had nothing: eleven lanes in
// buildReconcilers, fifteen `invariant:operating` bindings in the declarations,
// and no code comparing them — because the registry IMPORTS the reconciler in
// order to declare it, so a check written on either side is a cycle. This package
// holds both, which is the same reason command_wiring_test.go lives here.
//
// What the first run of this test found, before it could be made to pass:
//
//	apl-overlay      ran under a name declared as `apl-overlay-sync`
//	token-inventory  ran under a name declared as `expiry-inventory`
//	observe          ran with no declaration naming it
//	linode-creds     ran with no declaration naming it
//
// Seven of eleven matched. That is why nobody noticed the other four: a table
// that is mostly right reads as a table that is right.
//
// THE LANES ARE THE SOURCE OF TRUTH FOR NAMES. A lane's name is what the metrics
// publish (llz_reconcile_up{lane=...}), what the alerts select on, and what an
// operator reads in a dashboard. So where the two disagreed, the DECLARATION was
// corrected — renaming a lane would have broken every alert expression that
// already names it.
// ────────────────────────────────────────────────────────────────────────────

import (
	"sort"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/reconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension/registry"
)

// invariantExempt names `invariant:operating` bindings that are deliberately NOT
// reconcile-daemon lanes, with the argument. An invariant is "must hold
// continuously"; the daemon is one way to hold something, not the only one.
var invariantExempt = map[string]string{
	// Scheduled checks, not daemon lanes: these run from llz-scheduled-checks.yml
	// on a cron, because what they measure (a rotation SLA, component readiness)
	// changes on the scale of days and a 30s in-cluster loop would be pure cost.
	"health-sla/rotation-sla":        "runs from llz-scheduled-checks.yml on a cron, not in the daemon",
	"health-sla/component-readiness": "runs from llz-scheduled-checks.yml on a cron, not in the daemon",

	// Held by a Kubernetes object rather than by a loop. The SSE-C gateway is a
	// DaemonSet: the invariant "objects land encrypted" is maintained by the proxy
	// being in the data path, and the daemon has nothing to reconcile toward it.
	"obj-encryption/obj-proxy": "held by the objProxy DaemonSet being in the path, not by a reconcile loop",

	// A static posture gate over Terraform declarations. It holds continuously in
	// the sense that every root must always declare encryption, and it is checked
	// at PR time by `llz ci at-rest-guard` — there is no cluster state to drive.
	"posture-at-rest/": "a static Terraform posture gate; nothing in-cluster to reconcile",
}

// EVERY LANE NAMES A DECLARED INVARIANT.
//
// A lane with no declaration is a continuously-running cluster mutator outside the
// model: invisible to `llz extension list`, to enablement, and to the capability
// fence. That is the most dangerous thing this framework can fail to see, because
// an invariant lane runs forever and acts without a human present.
func TestEveryReconcilerLaneNamesADeclaredInvariant(t *testing.T) {
	declared := declaredInvariants(t)

	var unnamed, unknown []string
	lanes := reconciler.ReconcilerLanes()
	if len(lanes) == 0 {
		t.Fatal("the daemon reported no lanes — this test would pass over an empty set")
	}
	for _, l := range lanes {
		switch {
		case l.Extension == "" || l.Binding == "":
			unnamed = append(unnamed, l.Lane)
		case !declared[l.Extension+"/"+l.Binding]:
			unknown = append(unknown, l.Lane+" claims "+l.Extension+"/"+l.Binding)
		}
	}
	sort.Strings(unnamed)
	sort.Strings(unknown)

	if len(unnamed) > 0 {
		t.Errorf("%d reconcile lane(s) claim no declaration:\n\t%s\n"+
			"\tA lane outside the model runs forever, mutates a cluster, and is invisible "+
			"to enablement and to `llz extension list`.",
			len(unnamed), strings.Join(unnamed, "\n\t"))
	}
	if len(unknown) > 0 {
		t.Errorf("%d reconcile lane(s) name a binding no extension declares:\n\t%s\n"+
			"\tThe lane name is what the metrics publish and the alerts select on, so fix "+
			"the DECLARATION rather than renaming the lane.",
			len(unknown), strings.Join(unknown, "\n\t"))
	}
	t.Logf("reconcile lanes: %d, all declared", len(lanes))
}

// AND EVERY DECLARED INVARIANT IS A LANE, OR IS EXEMPT WITH A REASON.
//
// This is the direction that catches a declaration claiming something the binary
// does not do — an invariant nobody maintains reads exactly like one that holds.
func TestEveryDeclaredInvariantIsALaneOrExempt(t *testing.T) {
	running := map[string]bool{}
	for _, l := range reconciler.ReconcilerLanes() {
		running[l.Extension+"/"+l.Binding] = true
	}

	declared := declaredInvariants(t)
	var missing []string
	for key := range declared {
		if running[key] {
			continue
		}
		if _, ok := invariantExempt[key]; ok {
			continue
		}
		missing = append(missing, key)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d declared invariant(s) are maintained by nothing:\n\t%s\n"+
			"\tAn invariant is \"must hold continuously\". One with no lane and no argument "+
			"is a claim the binary does not keep. Add a lane, or add it to invariantExempt "+
			"with what holds it instead.",
			len(missing), strings.Join(missing, "\n\t"))
	}

	// The ratchet half: an exemption for a binding that is now a lane, or that
	// stopped being declared, is a stale allowance.
	var stale []string
	for key, why := range invariantExempt {
		if running[key] || !declared[key] {
			stale = append(stale, key)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("invariantExempt[%q] has no reason", key)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("%d exemption(s) no longer apply — DELETE them from invariantExempt:\n\t%s",
			len(stale), strings.Join(stale, "\n\t"))
	}

	t.Logf("declared invariants: %d, running as lanes: %d, exempt: %d",
		len(declared), len(running), len(invariantExempt))
}

// declaredInvariants keys every `invariant:operating` binding as
// "<extension>/<binding name>". An unnamed binding keys as "<extension>/", which
// is why posture-at-rest's exemption reads that way.
func declaredInvariants(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, e := range registry.All() {
		for _, b := range e.Bindings {
			if b.Kind == extension.Invariant {
				out[e.Name+"/"+b.Name] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no invariant bindings found — the registry moved or the model changed")
	}
	return out
}
